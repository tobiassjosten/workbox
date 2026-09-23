package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/state"
)

// testApp builds an App with working hours 06:00–23:00 and the default 30-minute
// idle timeout, backed by fakes and a fixed clock.
func testApp(t *testing.T, vm compute.State, now time.Time) (*App, *state.Fake, *bytes.Buffer) {
	t.Helper()
	sc, err := schedule.New("06:00", "23:00", "Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Name: "workbox"}
	cfg.Schedule = config.Schedule{
		Timezone:     "Europe/Stockholm",
		WorkingHours: &config.WorkingHours{Start: "06:00", End: "23:00"},
	}
	cfg.Tailscale.SSHTarget = "workbox"
	store := state.NewFake()
	buf := &bytes.Buffer{}
	app := &App{
		Cfg:     cfg,
		Sched:   sc,
		Compute: compute.NewFake(vm),
		Store:   store,
		Now:     func() time.Time { return now },
		Out:     buf,
	}
	return app, store, buf
}

// disableIdle turns off idle shutdown on an app's config.
func disableIdle(app *App) {
	zero := 0
	app.Cfg.Schedule.IdleTimeoutMinutes = &zero
}

func localTime(t *testing.T, y int, m time.Month, d, h, min int) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("Europe/Stockholm")
	return time.Date(y, m, d, h, min, 0, 0, loc)
}

func TestWakeSetsGraceHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, store, buf := testApp(t, compute.Suspended, now)
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || doc.Hold.State != schedule.Awake {
		t.Fatalf("expected a keep-awake grace hold, got %+v", doc.Hold)
	}
	// Grace is the default idle timeout (30m) plus the default SSH wait (3m).
	if !doc.Hold.End.Equal(now.Add(33 * time.Minute)) {
		t.Errorf("grace hold end = %v, want now+33m", doc.Hold.End)
	}
	want := "Keep-awake hold set until Tue 2026-06-16 00:03 CEST (grace window).\n"
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("wake output missing %q\ngot:\n%s", want, out)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("VM state = %v, want Running", s)
	}
}

func TestWakeKeepsLongerHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, store, buf := testApp(t, compute.Suspended, now)
	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || !doc.Hold.End.Equal(now.Add(3*time.Hour)) {
		t.Errorf("wake shortened the existing hold; got %+v, want end now+3h", doc.Hold)
	}
	// The full line: wake must not label the user's own hold a grace window.
	want := "A longer keep-awake hold is already in place until Tue 2026-06-16 02:30 CEST.\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("wake output missing %q\ngot:\n%s", want, buf.String())
	}
}

func TestWakeCancelsInEffectScheduledSleep(t *testing.T) {
	// A scheduled sleep is currently active; waking must clear it so the
	// reconciler does not immediately re-suspend.
	now := localTime(t, 2026, 6, 15, 21, 0)
	app, store, buf := testApp(t, compute.Suspended, now)
	active := &schedule.Span{
		Start: now.Add(-time.Hour),
		End:   localTime(t, 2026, 6, 16, 6, 0),
		State: schedule.Asleep,
	}
	if err := store.Save(context.Background(), &state.Document{Sleep: active}); err != nil {
		t.Fatal(err)
	}
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep != nil {
		t.Errorf("in-effect scheduled sleep should be cleared on wake, got %+v", doc.Sleep)
	}
	if !strings.Contains(buf.String(), "Cancelled the scheduled sleep at") {
		t.Errorf("wake should report the cancelled sleep, got %q", buf.String())
	}
}

func TestWakeKeepsFutureScheduledSleep(t *testing.T) {
	// A scheduled sleep set for later today is not in effect yet; waking now must
	// leave it in place.
	now := localTime(t, 2026, 6, 15, 15, 0)
	future := &schedule.Span{
		Start: localTime(t, 2026, 6, 15, 20, 0),
		End:   localTime(t, 2026, 6, 16, 6, 0),
		State: schedule.Asleep,
	}
	app, store, buf := testApp(t, compute.Suspended, now)
	if err := store.Save(context.Background(), &state.Document{Sleep: future}); err != nil {
		t.Fatal(err)
	}
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep == nil {
		t.Error("future scheduled sleep should survive a wake")
	}
	// ...and the user is told it will still suspend them.
	want := "Scheduled sleep still set for Mon 2026-06-15 20:00 CEST (`workbox cancel` calls it off — and clears any keep-awake hold).\n"
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("wake output missing %q\ngot:\n%s", want, out)
	}
}

func TestWakeIdleDisabledNoHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30)
	app, store, _ := testApp(t, compute.Suspended, now)
	disableIdle(app)
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold != nil {
		t.Errorf("no grace hold expected when idle shutdown is disabled, got %+v", doc.Hold)
	}
}

// With idle shutdown off there is no grace hold, so cancelling an in-effect
// scheduled sleep empties the document — which must be deleted, not saved
// span-less (the reconciler only collects documents that still hold a span).
// Wake and KeepAwake persist before resuming, so a store failure must leave the
// VM alone rather than running with no hold recorded.
func TestWakeAndKeepAwakeStoreFailureDoesNotWake(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30)
	for _, tc := range []struct {
		name string
		run  func(*App) error
	}{
		{"wake", func(a *App) error { return a.Wake(context.Background()) }},
		{"keep-awake", func(a *App) error { return a.KeepAwake(context.Background(), time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, store, _ := testApp(t, compute.Suspended, now)
			// Fail the write alone: a failing read would abort before the
			// ordering under test is reached.
			store.SaveErr = errors.New("firestore unavailable")
			if err := tc.run(app); err == nil {
				t.Fatal("expected the store error")
			}
			if s, _ := app.Compute.Status(context.Background()); s != compute.Suspended {
				t.Errorf("VM state = %v, want Suspended (the hold was never stored)", s)
			}
		})
	}
}

func TestWakeClearsDocumentLeftWithoutSpans(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30)
	app, store, _ := testApp(t, compute.Suspended, now)
	disableIdle(app)
	doc, _ := store.Load(context.Background())
	doc.Set(state.KindSleep, schedule.Span{
		Start: now.Add(-time.Minute), End: now.Add(time.Hour), State: schedule.Asleep,
	})
	if err := store.Save(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.Doc != nil {
		t.Errorf("Wake should delete the document rather than save it span-less, got %+v", store.Doc)
	}
}

func TestWakeComputeFailurePreservesHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30)
	app, store, _ := testApp(t, compute.Suspended, now)
	app.Compute.(*compute.Fake).Err = errors.New("transient GCP error")
	if err := app.Wake(context.Background()); err == nil {
		t.Fatal("expected error from failed compute.Wake, got nil")
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || doc.Hold.State != schedule.Awake {
		t.Errorf("grace hold must persist when compute fails; got %+v", doc.Hold)
	}
}

// Sleep clears state only after the suspend lands, so a failed suspend leaves
// the hold and scheduled sleep in place and reports nothing as cleared.
func TestSleepComputeFailurePreservesState(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	app.Compute.(*compute.Fake).Err = errors.New("transient GCP error")
	if err := app.Sleep(context.Background()); err == nil {
		t.Fatal("expected error from failed compute.Sleep, got nil")
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || doc.Sleep == nil {
		t.Errorf("a failed suspend must leave hold and scheduled sleep in place; got %+v", doc)
	}
	if out := buf.String(); out != "" {
		t.Errorf("a failed suspend must report nothing as cleared; got:\n%s", out)
	}
}

// Sleep suspends before touching Firestore, so a state-store failure cannot
// leave a billable VM running.
func TestSleepStoreFailureStillSuspends(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 22, 0)
	app, store, _ := testApp(t, compute.Running, now)
	store.Err = errors.New("firestore unavailable")
	err := app.Sleep(context.Background())
	want := "workbox is asleep, but any keep-awake hold or scheduled sleep could not be cleared (run `workbox cancel` to retry): firestore unavailable"
	if err == nil || err.Error() != want {
		t.Fatalf("Sleep error = %v, want %q", err, want)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Suspended {
		t.Errorf("VM state = %v, want Suspended despite the store failure", s)
	}
}

// A failed state read must not stop the clear: the state change does not depend
// on the reporting half.
func TestSleepAndCancelClearDespiteReadFailure(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 22, 0)
	const want = "Cleared any keep-awake hold and scheduled sleep (could not read what was in effect: firestore read failed).\n"

	t.Run("sleep", func(t *testing.T) {
		app, store, buf := testApp(t, compute.Running, now)
		store.Doc = &state.Document{Hold: &schedule.Span{Start: now, End: now.Add(time.Hour), State: schedule.Awake}}
		store.LoadErr = errors.New("firestore read failed")
		if err := app.Sleep(context.Background()); err != nil {
			t.Fatalf("Sleep = %v, want nil", err)
		}
		if store.Doc != nil {
			t.Errorf("Sleep left the document in place: %+v", store.Doc)
		}
		if out := buf.String(); !strings.Contains(out, want) || !strings.Contains(out, "workbox is asleep.\n") {
			t.Errorf("Sleep output =\n%s\nwant %q and the asleep line", out, want)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		app, store, buf := testApp(t, compute.Running, now)
		store.LoadErr = errors.New("firestore read failed")
		if err := app.Cancel(context.Background()); err != nil {
			t.Fatalf("Cancel = %v, want nil", err)
		}
		if store.Doc != nil {
			t.Errorf("Cancel left the document in place: %+v", store.Doc)
		}
		if out := buf.String(); !strings.Contains(out, want) {
			t.Errorf("Cancel output =\n%s\nwant %q", out, want)
		}
	})
}

// A manual sleep also discards a *future* scheduled sleep, which only the
// cancelled-sleep line tells the user about.
func TestSleepReportsDiscardedScheduledSleep(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	if err := app.Sleep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if doc, _ := store.Load(context.Background()); doc.Sleep != nil {
		t.Errorf("manual sleep should discard the scheduled sleep, got %+v", doc.Sleep)
	}
	if out := buf.String(); !strings.Contains(out, "Cancelled the scheduled sleep at") {
		t.Errorf("sleep should report the scheduled sleep it discarded\ngot:\n%s", out)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Suspended {
		t.Errorf("VM state = %v, want Suspended", s)
	}
}

func TestSleepAtSchedulesSleep(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep == nil || doc.Sleep.State != schedule.Asleep {
		t.Fatalf("expected a scheduled sleep, got %+v", doc.Sleep)
	}
	if want := localTime(t, 2026, 6, 15, 20, 0); !doc.Sleep.Start.Equal(want) {
		t.Errorf("scheduled sleep start = %v, want %v", doc.Sleep.Start, want)
	}
	want := "Scheduled sleep: workbox suspends at Mon 2026-06-15 20:00 CEST and is held asleep until Tue 2026-06-16 06:00 CEST; it never wakes on its own — run `workbox wake` to resume.\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("output missing %q, got %q", want, buf.String())
	}
	// sleep HH:MM must not change VM state.
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("VM state = %v, want unchanged Running", s)
	}
}

func TestSleepAtReportsReplacedSleep(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := app.SleepAt(context.Background(), "22:30"); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "Replaced the scheduled sleep at") {
		t.Errorf("replacing a scheduled sleep should be reported\ngot:\n%s", out)
	}
	doc, _ := store.Load(context.Background())
	if want := localTime(t, 2026, 6, 15, 22, 30); doc.Sleep == nil || !doc.Sleep.Start.Equal(want) {
		t.Errorf("scheduled sleep = %+v, want start %v", doc.Sleep, want)
	}

	// Re-scheduling the same time is not a replacement.
	buf.Reset()
	if err := app.SleepAt(context.Background(), "22:30"); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); strings.Contains(out, "Replaced") {
		t.Errorf("re-scheduling the same time should not report a replacement\ngot:\n%s", out)
	}
}

func TestNormalizeHHMM(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2000", "20:00"},
		{"0630", "06:30"},
		{"20:00", "20:00"},
		{"200x", "200x"},
		{"200", "200"},
		{"20000", "20000"},
	} {
		if got := normalizeHHMM(tc.in); got != tc.want {
			t.Errorf("normalizeHHMM(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A bad time is reported as typed, not in its normalized form.
func TestSleepAtRejectsBadTimeAsTyped(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, _, _ := testApp(t, compute.Running, now)
	err := app.SleepAt(context.Background(), "2560")
	if want := `invalid time "2560": want HH:MM or HHMM, 00:00-23:59`; err == nil || err.Error() != want {
		t.Errorf("SleepAt(2560) error = %v, want %q", err, want)
	}
}

func TestSleepAtAcceptsColonlessTime(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, _ := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "2000"); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if want := localTime(t, 2026, 6, 15, 20, 0); doc.Sleep == nil || !doc.Sleep.Start.Equal(want) {
		t.Errorf("scheduled sleep = %+v, want start %v", doc.Sleep, want)
	}
}

// A hold that outlasts the scheduled start is warned about; one that ends
// before it is not (the sleep does not override it).
func TestSleepAtWarnsWhenHoldIsSuperseded(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	const warning = "The keep-awake hold until Mon 2026-06-15 21:00 CEST stays in place, but the scheduled sleep wins from Mon 2026-06-15 20:00 CEST.\n"

	app, _, buf := testApp(t, compute.Running, now)
	if err := app.KeepAwake(context.Background(), 6*time.Hour); err != nil { // until 21:00
		t.Fatal(err)
	}
	buf.Reset()
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), warning) {
		t.Errorf("SleepAt output missing %q\ngot:\n%s", warning, buf.String())
	}

	app, _, buf = testApp(t, compute.Running, now)
	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil { // until 18:00
		t.Fatal(err)
	}
	buf.Reset()
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "stays in place") {
		t.Errorf("SleepAt warned about a hold that ends before the sleep\ngot:\n%s", buf.String())
	}
}

func TestSleepAtKeepsHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, _ := testApp(t, compute.Running, now)
	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || !doc.Hold.End.Equal(now.Add(3*time.Hour)) {
		t.Errorf("scheduled sleep should leave the keep-awake hold alone, got %+v", doc.Hold)
	}
	if doc.Sleep == nil {
		t.Error("scheduled sleep not stored")
	}
}

func TestShortKeepAwakeDoesNotCancelSleepViaPreservedHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	// 6h hold (to 21:00), then a 20:00 sleep that wins where they overlap.
	if err := app.KeepAwake(context.Background(), 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	// A 1h keep-awake keeps the longer hold but must not cancel the sleep.
	buf.Reset()
	if err := app.KeepAwake(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep == nil {
		t.Error("a short keep-awake cancelled the scheduled sleep via the preserved hold")
	}
	// The kept hold does not protect past 20:00; the user must be told.
	want := "Scheduled sleep still set for Mon 2026-06-15 20:00 CEST (`workbox cancel` calls it off — and clears any keep-awake hold).\n"
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("keep-awake output missing %q\ngot:\n%s", want, out)
	}
}

func TestKeepAwakeKeepsLaterScheduledSleep(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, _ := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	// A 3h hold ends at 18:00, before the 20:00 sleep: both stand.
	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep == nil {
		t.Error("keep-awake ending before the scheduled sleep should keep it")
	}
}

// The "nothing would auto-suspend anyway" note is only true when no scheduled
// sleep survives the call — a sleep outranks the hold.
func TestKeepAwakeRedundancyNote(t *testing.T) {
	const note = "idle shutdown is disabled, so nothing would auto-suspend anyway"
	now := localTime(t, 2026, 6, 15, 15, 0)

	t.Run("no sleep: the note stands", func(t *testing.T) {
		app, _, buf := testApp(t, compute.Running, now)
		disableIdle(app)
		if err := app.KeepAwake(context.Background(), time.Hour); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), note) {
			t.Errorf("keep-awake output missing the note\ngot:\n%s", buf.String())
		}
	})

	t.Run("preserved hold keeps the note", func(t *testing.T) {
		app, _, buf := testApp(t, compute.Running, now)
		disableIdle(app)
		if err := app.KeepAwake(context.Background(), 6*time.Hour); err != nil {
			t.Fatal(err)
		}
		buf.Reset()
		if err := app.KeepAwake(context.Background(), time.Hour); err != nil {
			t.Fatal(err)
		}
		want := "A longer keep-awake hold is already in place until Mon 2026-06-15 21:00 CEST (" + note + ").\n"
		if got := buf.String(); !strings.Contains(got, want) {
			t.Errorf("keep-awake output missing %q\ngot:\n%s", want, got)
		}
	})

	t.Run("surviving sleep: no note", func(t *testing.T) {
		app, store, buf := testApp(t, compute.Running, now)
		disableIdle(app)
		sleep := &schedule.Span{Start: localTime(t, 2026, 6, 15, 23, 0), End: localTime(t, 2026, 6, 16, 6, 0), State: schedule.Asleep}
		if err := store.Save(context.Background(), &state.Document{Sleep: sleep}); err != nil {
			t.Fatal(err)
		}
		if err := app.KeepAwake(context.Background(), time.Hour); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if strings.Contains(out, note) {
			t.Errorf("the note is wrong while a scheduled sleep survives\ngot:\n%s", out)
		}
		if !strings.Contains(out, "Scheduled sleep still set for") {
			t.Errorf("keep-awake should report the surviving sleep\ngot:\n%s", out)
		}
	})
}

// A hold ending exactly when the sleep starts does not overlap it ([start, end)
// spans), so the sleep stands.
func TestKeepAwakeEndingAtSleepStartKeepsSleep(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, _ := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	if err := app.KeepAwake(context.Background(), 5*time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep == nil {
		t.Error("a hold ending at the sleep's start should keep the sleep")
	}
}

func TestKeepAwakeSetsHoldWakesAndClearsOverlappingSleep(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Suspended, now)
	// A scheduled sleep at 20:00 exists first; a 6h hold runs past it.
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	if err := app.KeepAwake(context.Background(), 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || !doc.Hold.End.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("keep-awake hold = %+v, want end now+6h", doc.Hold)
	}
	if doc.Sleep != nil {
		t.Errorf("keep-awake should clear the overlapping scheduled sleep, got %+v", doc.Sleep)
	}
	if want := "Keep-awake hold set until Mon 2026-06-15 21:00 CEST.\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("keep-awake output missing %q\ngot:\n%s", want, buf.String())
	}
	if !strings.Contains(buf.String(), "Cancelled the scheduled sleep at") {
		t.Errorf("keep-awake should report the cancelled sleep, got %q", buf.String())
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("VM state = %v, want Running", s)
	}
}

func TestKeepAwakeNeverShortensExistingHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 1, 0)
	app, store, buf := testApp(t, compute.Suspended, now)
	// Establish a long hold (4h) first.
	if err := app.KeepAwake(context.Background(), 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	// A shorter keep-awake (1h) must not shorten the existing 4h hold.
	if err := app.KeepAwake(context.Background(), 1*time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || !doc.Hold.End.Equal(now.Add(4*time.Hour)) {
		t.Errorf("shorter keep-awake shortened the existing hold; got %+v, want end now+4h", doc.Hold)
	}
	if out := buf.String(); !strings.Contains(out, "A longer keep-awake hold is already in place") {
		t.Errorf("the superseded request should be explained\ngot:\n%s", out)
	}
}

func TestCancelClears(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "20:00"); err != nil {
		t.Fatal(err)
	}
	if doc, _ := store.Load(context.Background()); doc.Sleep == nil {
		t.Fatal("precondition: expected a scheduled sleep before cancel")
	}
	if err := app.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep != nil || doc.Hold != nil {
		t.Errorf("cancel should clear everything, got %+v", doc)
	}
	if out := buf.String(); !strings.Contains(out, "Cancelled the scheduled sleep at") {
		t.Errorf("cancel should report what it cleared\ngot:\n%s", out)
	}
}

func TestCancelWithNothingInEffect(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	// Only an expired hold is stored: invisible to the pruned view, but the
	// document must still be deleted.
	expired := &schedule.Span{Start: now.Add(-3 * time.Hour), End: now.Add(-time.Hour), State: schedule.Awake}
	if err := store.Save(context.Background(), &state.Document{Hold: expired}); err != nil {
		t.Fatal(err)
	}
	if err := app.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.Doc != nil {
		t.Errorf("cancel should delete a document of expired spans, got %+v", store.Doc)
	}
	want := "Nothing to clear: no scheduled sleep or keep-awake hold was in effect.\n"
	if out := buf.String(); out != want {
		t.Errorf("cancel output = %q, want %q", out, want)
	}
}

func TestPrintStatusWithinWorkingHours(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 12, 0) // within working hours
	app, _, buf := testApp(t, compute.Running, now)
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"VM:              RUNNING\n",
		"Working hours:   06:00–23:00 every day Europe/Stockholm (within — idle shutdown paused)\n",
		"Idle shutdown:   after 30m of inactivity (outside working hours)\n",
		"Auto-suspend:    no — within working hours\n",
		"Tailscale/SSH:   target workbox (VM running)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestPrintStatusIdleWithActivity(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(-5 * time.Minute), ActiveOK: true}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"outside — idle shutdown active",
		"Activity:        last active",
		"idle 5m",
		"Auto-suspend:    no — recently active",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, out)
		}
	}
}

// A last-active ahead of the local clock (VM/laptop skew) must read as zero
// idle, never as a negative duration.
func TestPrintStatusIdleClampedOnClockSkew(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(time.Minute), ActiveOK: true}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "idle 0m") {
		t.Errorf("PrintStatus output missing the clamped idle time\ngot:\n%s", out)
	}

	app, _, buf = testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(time.Minute), ActiveOK: true}
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var s StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Activity == nil || s.Activity.IdleSeconds != 0 {
		t.Errorf("idle_seconds = %+v, want 0", s.Activity)
	}
}

func TestPrintStatusNoActivityReported(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 12, 0) // within working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "Activity:        none reported") {
		t.Errorf("PrintStatus output missing the no-report line\ngot:\n%s", out)
	}
}

// Emitter wired but never reported, outside working hours: the VM is about to be
// suspended, and both lines must say so.
func TestPrintStatusNoActivityReportedSuspends(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Activity:        none reported",
		"Auto-suspend:    yes — no activity reported",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, out)
		}
	}

	app, _, buf = testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{}
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var s StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if !s.AutoSuspend.Suspend || s.AutoSuspend.Reason != schedule.ReasonNoActivity {
		t.Errorf("auto_suspend = %+v, want suspend with reason %q", s.AutoSuspend, schedule.ReasonNoActivity)
	}
}

func TestPrintStatusActivityUnavailable(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveErr: errors.New("permission denied")}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Activity:        unavailable (permission denied)",
		"Auto-suspend:    unknown — activity or start time could not be read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, out)
		}
	}

	// The same failure is reported in JSON, where it must not look like "no
	// activity reported".
	app, _, buf = testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveErr: errors.New("permission denied")}
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["activity_error"] != "permission denied" {
		t.Errorf("activity_error = %v, want %q", got["activity_error"], "permission denied")
	}
}

func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{100 * time.Second, "2m"},
		{45 * time.Minute, "45m"},
		{90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		{0, "0m"},
	} {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDaysLabel(t *testing.T) {
	wd := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	for _, tc := range []struct {
		name string
		days []time.Weekday
		want string
	}{
		{"none", nil, "every day"},
		{"all", append(wd, time.Saturday, time.Sunday), "every day"},
		{"weekdays", wd, "Mon–Fri"},
		{"weekend", []time.Weekday{time.Sunday, time.Saturday}, "Sat–Sun"},
		{"list in calendar order", []time.Weekday{time.Friday, time.Monday}, "Mon, Fri"},
	} {
		if got := daysLabel(tc.days); got != tc.want {
			t.Errorf("%s: daysLabel(%v) = %q, want %q", tc.name, tc.days, got, tc.want)
		}
	}
}

func TestPrintStatusWithoutWorkingHours(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 12, 0)
	app, _, buf := testApp(t, compute.Running, now)
	sc, err := schedule.Disabled("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	app.Sched = sc
	app.Cfg.Schedule.WorkingHours = nil
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Working hours:   none (times in Europe/Stockholm; idle shutdown always applies)",
		"Idle shutdown:   after 30m of inactivity\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, out)
		}
	}
}

// The human-readable schedule lines for each working-hours/idle combination.
func TestPrintStatusScheduleLines(t *testing.T) {
	disabledSched, err := schedule.Disabled("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	off := false
	for _, tc := range []struct {
		name  string
		setup func(*App)
		want  []string
	}{
		{
			name:  "outside hours, idle disabled",
			setup: disableIdle,
			want: []string{
				"(outside — idle shutdown disabled)",
				"Idle shutdown:   disabled\n",
				"Auto-suspend:    no — idle shutdown disabled\n",
			},
		},
		{
			name: "no window, idle disabled",
			setup: func(a *App) {
				disableIdle(a)
				a.Sched, a.Cfg.Schedule.WorkingHours = disabledSched, nil
			},
			want: []string{"Working hours:   none (times in Europe/Stockholm)\n", "Idle shutdown:   disabled\n"},
		},
		{
			name: "window disabled in config",
			setup: func(a *App) {
				a.Sched = disabledSched
				a.Cfg.Schedule.WorkingHours.Enabled = &off
			},
			want: []string{"Working hours:   disabled in config (times in Europe/Stockholm; idle shutdown always applies)\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
			app, _, buf := testApp(t, compute.Running, now)
			tc.setup(app)
			if err := app.PrintStatus(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if out := buf.String(); !strings.Contains(out, want) {
					t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, out)
				}
			}
		})
	}
}

// An unreadable activity value leaves the boot grace to decide here (the
// reconciler instead retries the tick), while still reporting the read failure.
func TestPrintStatusBootGraceWithActivityError(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveErr: errors.New("parsing guest attribute"), LastStartAt: now.Add(-5 * time.Minute)}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Activity:        unavailable (parsing guest attribute); run `workbox doctor`\n",
		"Auto-suspend:    no — recently started (earliest idle suspend Mon 2026-06-15 23:55 CEST)\n",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
		}
	}
}

// A failed start-time read makes an idle verdict unknown (the start could have
// meant boot grace) without hiding an activity value that was read.
func TestPrintStatusStartReadError(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(-2 * time.Hour), ActiveOK: true, LastStartErr: errors.New("getting instance")}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Activity:        last active Mon 2026-06-15 21:30 CEST (idle 2h)\n",
		"Last start:      unavailable (getting instance); retry, and run `workbox doctor` if it persists\n",
		"Auto-suspend:    unknown — activity or start time could not be read\n",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
		}
	}

	// No activity reported either: the start time alone could have meant boot
	// grace, so the verdict is unknown rather than "no activity".
	app, _, buf = testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{LastStartErr: errors.New("getting instance")}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Activity:        none reported; run `workbox doctor` if this persists\n",
		"Last start:      unavailable (getting instance); retry, and run `workbox doctor` if it persists\n",
		"Auto-suspend:    unknown — activity or start time could not be read\n",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
		}
	}

	// Recent activity decides on its own; the missing start time is moot.
	app, _, buf = testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(-5 * time.Minute), ActiveOK: true, LastStartErr: errors.New("getting instance")}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "Auto-suspend:    no — recently active (earliest idle suspend Mon 2026-06-15 23:55 CEST)\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
	}

	// The failed read is reported in JSON too, so a consumer can tell it from
	// "nothing reported".
	app, _, buf = testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(-2 * time.Hour), ActiveOK: true, LastStartErr: errors.New("getting instance")}
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["last_start_error"] != "getting instance" {
		t.Errorf("last_start_error = %v, want %q", got["last_start_error"], "getting instance")
	}
}

// A value workbox disregards is shown as ignored, and the verdict counts no
// activity — the reconciler does not count such a value as activity either.
func TestPrintStatusIgnoredActivity(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	for _, tc := range []struct {
		name   string
		act    compute.FakeActivity
		reason string
	}{
		{"far future", compute.FakeActivity{ActiveAt: now.Add(10 * time.Minute), ActiveOK: true},
			"last active Mon 2026-06-15 23:40 CEST is in the future"},
		{"not a timestamp", compute.FakeActivity{ActiveErr: fmt.Errorf("guest attribute: %w", compute.ErrInvalidActivity)},
			"not a usable unix timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, buf := testApp(t, compute.Running, now)
			app.Activity = tc.act
			if err := app.PrintStatus(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"Activity:        ignored (" + tc.reason + "); run `workbox doctor`\n",
				"Auto-suspend:    yes — no activity reported\n",
			} {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
				}
			}

			app, _, buf = testApp(t, compute.Running, now)
			app.Activity = tc.act
			if err := app.PrintStatusJSON(context.Background()); err != nil {
				t.Fatal(err)
			}
			var s StatusJSON
			if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
				t.Fatal(err)
			}
			if s.Activity != nil || s.ActivityIgnored != tc.reason || s.AutoSuspend.Reason != schedule.ReasonNoActivity {
				t.Errorf("JSON activity=%+v activity_ignored=%q reason=%q, want none, %q, %q",
					s.Activity, s.ActivityIgnored, s.AutoSuspend.Reason, tc.reason, schedule.ReasonNoActivity)
			}
		})
	}
}

// Without an activity reader the CLI cannot tell "no activity" from "unread".
func TestStatusJSONNoActivityReader(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var s StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.AutoSuspend.Suspend || s.AutoSuspend.Reason != ReasonActivityUnknown {
		t.Errorf("auto_suspend = %+v, want no-suspend %s", s.AutoSuspend, ReasonActivityUnknown)
	}
}

// A freshly booted VM with no activity yet is protected by the boot grace.
func TestPrintStatusBootGrace(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{LastStartAt: now.Add(-5 * time.Minute)}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "Auto-suspend:    no — recently started (earliest idle suspend Mon 2026-06-15 23:55 CEST)\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
	}
}

// The earliest idle suspend counts from the later of activity and last start.
func TestPrintStatusEarliestSuspendUsesLaterOfBoth(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{
		ActiveAt: now.Add(-20 * time.Minute), ActiveOK: true, LastStartAt: now.Add(-10 * time.Minute),
	}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "Auto-suspend:    no — recently active (earliest idle suspend Mon 2026-06-15 23:50 CEST)\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
	}
}

// A suspend time that would fall inside working hours is not named: the
// working-hours rule inhibits it.
func TestPrintStatusNoEarliestSuspendInsideWorkingHours(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 5, 45) // before the 06:00 window
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now, ActiveOK: true}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "Auto-suspend:    no — recently active\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
	}
}

// With idle shutdown off, the working-hours line agrees with the idle line.
func TestPrintStatusWithinHoursIdleDisabled(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 12, 0) // within working hours
	app, _, buf := testApp(t, compute.Running, now)
	disableIdle(app)
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(within — idle shutdown disabled)\n",
		"Idle shutdown:   disabled\n",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
		}
	}
}

// A scheduled sleep in effect shows its span, not a start time in the past.
func TestPrintStatusScheduledSleepInEffect(t *testing.T) {
	now := localTime(t, 2026, 6, 16, 2, 0)
	app, store, buf := testApp(t, compute.Running, now)
	sleep := &schedule.Span{Start: localTime(t, 2026, 6, 15, 23, 0), End: localTime(t, 2026, 6, 16, 6, 0), State: schedule.Asleep}
	if err := store.Save(context.Background(), &state.Document{Sleep: sleep}); err != nil {
		t.Fatal(err)
	}
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Auto-suspend:    yes — a scheduled sleep is due\n",
		"Scheduled sleep: in effect since Mon 2026-06-15 23:00 CEST until Tue 2026-06-16 06:00 CEST\n",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, buf.String())
		}
	}
}

// Every reason the status document can carry renders to its own verdict line.
func TestDescribeDecision(t *testing.T) {
	for _, tc := range []struct {
		reason  string
		suspend bool
		want    string
	}{
		{ReasonNotRunning, false, "n/a — VM is SUSPENDED"},
		{ReasonActivityUnknown, false, "unknown — activity or start time could not be read"},
		{schedule.ReasonScheduledSleep, true, "yes — a scheduled sleep is due"},
		{schedule.ReasonKeepAwake, false, "no — held awake"},
		{schedule.ReasonWorkingHours, false, "no — within working hours"},
		{schedule.ReasonIdleDisabled, false, "no — idle shutdown disabled"},
		{schedule.ReasonActive, false, "no — recently active"},
		{schedule.ReasonBootGrace, false, "no — recently started"},
		{schedule.ReasonNoActivity, true, "yes — no activity reported"},
		{schedule.ReasonIdle, true, "yes — idle past the timeout"},
	} {
		if got := describeDecision(tc.suspend, tc.reason, "SUSPENDED"); got != tc.want {
			t.Errorf("describeDecision(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

// The reconciler returns this CLI verdict verbatim for a VM that is not
// RUNNING, so the two sides must keep one spelling.
func TestReconcilerReturnsNotRunning(t *testing.T) {
	raw, err := os.ReadFile("../../infra/reconcile.yaml.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	if want := `return: "` + ReasonNotRunning + `"`; !strings.Contains(string(raw), want) {
		t.Errorf("infra/reconcile.yaml.tftpl no longer contains %s", want)
	}
}

func TestSleepSuspendsAndClears(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 22, 0)
	app, store, buf := testApp(t, compute.Running, now)
	// Pre-existing keep-awake hold that a manual sleep should clear.
	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.Sleep(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep != nil || doc.Hold != nil {
		t.Errorf("manual sleep should clear holds/scheduled sleep, got %+v", doc)
	}
	if out := buf.String(); !strings.Contains(out, "Cleared the keep-awake hold (was set until") {
		t.Errorf("sleep should report the hold it cleared\ngot:\n%s", out)
	}
}

// TestStatusJSONKeys pins the wire names of `status --json` by decoding into a
// generic map, so renaming a json tag breaks the test (a round-trip through
// StatusJSON would not).
func TestStatusJSONKeys(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30) // outside working hours
	app, store, buf := testApp(t, compute.Running, now)
	// Set for realism; the JSON days field comes from app.Sched below.
	app.Cfg.Schedule.WorkingHours.Days = []string{"mon", "tue", "wed", "thu", "fri"}
	sc, err := schedule.New("06:00", "23:00", "Europe/Stockholm",
		time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday)
	if err != nil {
		t.Fatal(err)
	}
	app.Sched = sc
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(-10 * time.Minute), ActiveOK: true, LastStartAt: now.Add(-2 * time.Hour)}
	hold := &schedule.Span{Start: now.Add(-time.Hour), End: now.Add(time.Hour), State: schedule.Awake}
	sleep := &schedule.Span{Start: now.Add(2 * time.Hour), End: now.Add(8 * time.Hour), State: schedule.Asleep}
	if err := store.Save(context.Background(), &state.Document{Hold: hold, Sleep: sleep}); err != nil {
		t.Fatal(err)
	}
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	span := func(sp *schedule.Span) map[string]any {
		return map[string]any{"start": sp.Start.Format(time.RFC3339), "end": sp.End.Format(time.RFC3339), "state": string(sp.State)}
	}
	want := map[string]any{
		"name": "workbox",
		"vm":   "RUNNING",
		"schedule": map[string]any{
			"timezone": "Europe/Stockholm",
			"working_hours": map[string]any{
				"enabled": true, "start": "06:00", "end": "23:00",
				"days": []any{"Mon", "Tue", "Wed", "Thu", "Fri"}, "within": false,
			},
			"idle_timeout_minutes": float64(30),
		},
		"auto_suspend": map[string]any{"suspend": false, "reason": "keep-awake"},
		"activity": map[string]any{
			"last_active":  now.Add(-10 * time.Minute).Format(time.RFC3339),
			"idle_seconds": float64(600),
		},
		"last_start":      now.Add(-2 * time.Hour).Format(time.RFC3339),
		"keep_awake":      span(hold),
		"scheduled_sleep": span(sleep),
		"ssh":             map[string]any{"target": "workbox", "available": true},
	}
	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("status JSON =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

// An ignored activity value appears under activity_ignored, never as activity.
func TestStatusJSONIgnoredActivityKeys(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 23, 30)
	app, _, buf := testApp(t, compute.Running, now)
	app.Activity = compute.FakeActivity{ActiveAt: now.Add(10 * time.Minute), ActiveOK: true}
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := "last active Mon 2026-06-15 23:40 CEST is in the future"; got["activity_ignored"] != want {
		t.Errorf("activity_ignored = %v, want %q", got["activity_ignored"], want)
	}
	if _, ok := got["activity"]; ok {
		t.Errorf("activity should be absent when the value is ignored, got %v", got["activity"])
	}
}

func TestStatusJSONStable(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 12, 0)
	app, _, buf := testApp(t, compute.Running, now)
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var s StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		t.Fatalf("status JSON did not parse: %v", err)
	}
	if s.VM != "RUNNING" {
		t.Errorf("VM = %q, want RUNNING", s.VM)
	}
	if s.AutoSuspend.Suspend || s.AutoSuspend.Reason != schedule.ReasonWorkingHours {
		t.Errorf("auto_suspend = %+v, want no-suspend working-hours", s.AutoSuspend)
	}
	if !s.Schedule.WorkingHours.Enabled || !s.Schedule.WorkingHours.Within {
		t.Errorf("working_hours = %+v, want within=true", s.Schedule.WorkingHours)
	}
	if s.Schedule.IdleTimeoutMinutes != 30 {
		t.Errorf("idle_timeout_minutes = %d, want 30", s.Schedule.IdleTimeoutMinutes)
	}
	if !s.SSH.Available {
		t.Error("SSH should be available when running")
	}
	// An every-day window carries no days list (omitempty).
	var generic map[string]any
	if err := json.Unmarshal(buf.Bytes(), &generic); err != nil {
		t.Fatal(err)
	}
	hours := generic["schedule"].(map[string]any)["working_hours"].(map[string]any)
	if _, ok := hours["days"]; ok {
		t.Errorf("working_hours.days = %v, want it absent for an every-day window", hours["days"])
	}
}

func TestStatusJSONSuspended(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 2, 0)
	app, _, buf := testApp(t, compute.Suspended, now)
	if err := app.PrintStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var s StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.SSH.Available {
		t.Error("SSH should be unavailable when suspended")
	}
	if s.SSH.Reason == "" {
		t.Error("expected a reason for SSH unavailability")
	}
	// Outside working hours with no activity, but already asleep: no verdict.
	if s.AutoSuspend.Suspend || s.AutoSuspend.Reason != ReasonNotRunning {
		t.Errorf("auto_suspend = %+v, want no-suspend %s", s.AutoSuspend, ReasonNotRunning)
	}
}

func TestSchedule(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	tests := []struct {
		name    string
		doc     *state.Document
		wantOut []string
	}{
		{
			name:    "no hold or scheduled sleep",
			wantOut: []string{"Working hours:   06:00–23:00", "Keep-awake:      none", "Scheduled sleep: none"},
		},
		{
			name:    "scheduled sleep",
			doc:     &state.Document{Sleep: &schedule.Span{Start: time.Date(2026, 6, 15, 20, 0, 0, 0, loc), End: time.Date(2026, 6, 16, 6, 0, 0, 0, loc), State: schedule.Asleep}},
			wantOut: []string{"Scheduled sleep: at", "20:00"},
		},
		{
			name:    "keep-awake",
			doc:     &state.Document{Hold: &schedule.Span{Start: time.Date(2026, 6, 15, 12, 0, 0, 0, loc), End: time.Date(2026, 6, 15, 23, 0, 0, 0, loc), State: schedule.Awake}},
			wantOut: []string{"Keep-awake:      until"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := localTime(t, 2026, 6, 15, 12, 0)
			app, store, buf := testApp(t, compute.Running, now)
			if tc.doc != nil {
				if err := store.Save(context.Background(), tc.doc); err != nil {
					t.Fatal(err)
				}
			}
			if err := app.Schedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("Schedule output missing %q\ngot:\n%s", want, out)
				}
			}
		})
	}
}
