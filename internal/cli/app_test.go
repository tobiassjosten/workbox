package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/state"
)

func testApp(t *testing.T, vm compute.State, now time.Time) (*App, *state.Fake, *bytes.Buffer) {
	t.Helper()
	sc, err := schedule.New("06:00", "23:00", "Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Name: "workbox"}
	cfg.Schedule = config.Schedule{Wake: "06:00", Sleep: "23:00", Timezone: "Europe/Stockholm"}
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

func localTime(t *testing.T, y int, m time.Month, d, h, min int) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("Europe/Stockholm")
	return time.Date(y, m, d, h, min, 0, 0, loc)
}

func TestWakeAtNightSetsHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 1, 0) // baseline asleep
	app, store, _ := testApp(t, compute.Suspended, now)
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || doc.Hold.State != schedule.Awake {
		t.Fatalf("expected a stay-awake hold, got %+v", doc.Hold)
	}
	// Hold must end at the next scheduled sleep (23:00 that day).
	wantEnd := localTime(t, 2026, 6, 15, 23, 0)
	if !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("hold end = %v, want %v", doc.Hold.End, wantEnd)
	}
	// Compute should now be running.
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("VM state = %v, want Running", s)
	}
}

func TestWakeDaytimeNoHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 10, 0) // baseline awake
	app, store, _ := testApp(t, compute.Suspended, now)
	if err := app.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold != nil {
		t.Errorf("no hold expected during baseline-awake hours, got %+v", doc.Hold)
	}
}

func TestWakeComputeFailurePreservesHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 1, 0) // baseline asleep — a hold will be set
	app, store, _ := testApp(t, compute.Suspended, now)
	app.Compute.(*compute.Fake).Err = errors.New("transient GCP error")
	err := app.Wake(context.Background())
	if err == nil {
		t.Fatal("expected error from failed compute.Wake, got nil")
	}
	// Hold must still be in the store: the reconciler uses it to recover.
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || doc.Hold.State != schedule.Awake {
		t.Errorf("hold must persist when compute fails; got %+v", doc.Hold)
	}
}

func TestSleepEveningSetsHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 22, 0) // baseline awake until 23:00
	app, store, _ := testApp(t, compute.Running, now)
	if err := app.Sleep(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || doc.Hold.State != schedule.Asleep {
		t.Fatalf("expected a stay-asleep hold, got %+v", doc.Hold)
	}
	// Hold must end at the next scheduled wake (06:00 the following day).
	wantEnd := localTime(t, 2026, 6, 16, 6, 0)
	if !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("hold end = %v, want %v", doc.Hold.End, wantEnd)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Suspended {
		t.Errorf("VM state = %v, want Suspended", s)
	}
}

func TestWakeAtBaselineIsNoOp(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 20, 0)
	app, store, buf := testApp(t, compute.Suspended, now)
	if err := app.WakeAt(context.Background(), "06:00"); err != nil {
		t.Fatalf("expected no error for baseline wake time, got %v", err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Wake != nil {
		t.Errorf("no override expected when wake-at equals baseline, got %+v", doc.Wake)
	}
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("expected informational output, got %q", buf.String())
	}
}

func TestSleepAtBaselineIsNoOp(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 15, 0)
	app, store, buf := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "23:00"); err != nil {
		t.Fatalf("expected no error for baseline sleep time, got %v", err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep != nil {
		t.Errorf("no override expected when sleep-at equals baseline, got %+v", doc.Sleep)
	}
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("expected informational output, got %q", buf.String())
	}
}

func TestWakeAtLateOverride(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 20, 0)
	app, store, buf := testApp(t, compute.Suspended, now)
	if err := app.WakeAt(context.Background(), "08:30"); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Wake == nil || doc.Wake.State != schedule.Asleep {
		t.Fatalf("expected a delayed-wake override, got %+v", doc.Wake)
	}
	out := buf.String()
	// Output must show the requested wake time (08:30) and the displaced baseline (06:00).
	if !strings.Contains(out, "08:30") {
		t.Errorf("output should mention requested wake time, got %q", out)
	}
	if !strings.Contains(out, "06:00") {
		t.Errorf("output should mention displaced baseline wake time, got %q", out)
	}
	// wake-at must not change VM state.
	if s, _ := app.Compute.Status(context.Background()); s != compute.Suspended {
		t.Errorf("VM state = %v, want unchanged Suspended", s)
	}
}

func TestWakeAtEarlyOverride(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 20, 0)
	app, store, buf := testApp(t, compute.Suspended, now)
	if err := app.WakeAt(context.Background(), "05:00"); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Wake == nil || doc.Wake.State != schedule.Awake {
		t.Fatalf("expected an early-wake override, got %+v", doc.Wake)
	}
	out := buf.String()
	// Output must show the early wake time (05:00) and the baseline it replaces (06:00).
	if !strings.Contains(out, "05:00") {
		t.Errorf("output should mention early wake time, got %q", out)
	}
	if !strings.Contains(out, "06:00") {
		t.Errorf("output should mention baseline wake time, got %q", out)
	}
}

func TestSleepAtCrossMidnight(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 21, 0)
	app, store, _ := testApp(t, compute.Running, now)
	if err := app.SleepAt(context.Background(), "01:30"); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Sleep == nil || doc.Sleep.State != schedule.Awake {
		t.Fatalf("expected an extend-awake override, got %+v", doc.Sleep)
	}
	want := localTime(t, 2026, 6, 16, 1, 30)
	if !doc.Sleep.End.Equal(want) {
		t.Errorf("sleep override end = %v, want %v", doc.Sleep.End, want)
	}
}

func TestKeepAwakeSetsHoldAndWakes(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 1, 0)
	app, store, _ := testApp(t, compute.Suspended, now)
	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || !doc.Hold.End.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("keep-awake hold = %+v, want end now+3h", doc.Hold)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("VM state = %v, want Running", s)
	}
}

func TestKeepAwakeNeverShortensExistingHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 1, 0)
	app, store, _ := testApp(t, compute.Suspended, now)
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
		t.Errorf("shorter keep-awake shortened the existing hold; got end=%v, want now+4h", doc.Hold.End)
	}
}

func TestKeepAwakeOverwritesAsleepHold(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 22, 0) // baseline awake; Sleep will set a stay-asleep hold
	app, store, _ := testApp(t, compute.Running, now)
	// Establish a stay-asleep hold (ends at next scheduled wake: 06:00 next day).
	if err := app.Sleep(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || doc.Hold.State != schedule.Asleep {
		t.Fatalf("precondition: expected stay-asleep hold, got %+v", doc.Hold)
	}
	// keep-awake must overwrite the stay-asleep hold, not preserve it.
	if err := app.KeepAwake(context.Background(), 1*time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ = store.Load(context.Background())
	if doc.Hold == nil || doc.Hold.State != schedule.Awake {
		t.Errorf("keep-awake should overwrite stay-asleep hold; got %+v", doc.Hold)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("VM state = %v, want Running", s)
	}
}

func TestKeepAwakePreservesExistingOverride(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 1, 0)
	app, store, _ := testApp(t, compute.Suspended, now)
	// Set a wake override first.
	if err := app.WakeAt(context.Background(), "05:00"); err != nil {
		t.Fatal(err)
	}
	// Now add a keep-awake hold; the wake override must survive.
	if err := app.KeepAwake(context.Background(), 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Wake == nil {
		t.Error("wake override was erased by keep-awake")
	}
	if doc.Hold == nil || doc.Hold.State != schedule.Awake {
		t.Errorf("expected stay-awake hold after keep-awake, got %+v", doc.Hold)
	}
}

func TestCancelOverride(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 21, 0)
	app, store, _ := testApp(t, compute.Running, now)
	_ = app.SleepAt(context.Background(), "01:30")
	if err := app.CancelOverride(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load(context.Background())
	if !doc.Empty() {
		t.Errorf("cancel-override should clear everything, got %+v", doc)
	}
}

func TestPrintStatus(t *testing.T) {
	now := localTime(t, 2026, 6, 15, 12, 0) // daytime, baseline awake
	app, _, buf := testApp(t, compute.Running, now)
	if err := app.PrintStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"RUNNING",
		"Desired:         awake",
		"06:00",
		"23:00",
		"Europe/Stockholm",
		"suspend",
		"Override:        none",
		"Hold:            none",
		"target workbox",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PrintStatus output missing %q\ngot:\n%s", want, out)
		}
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
	if s.VM != "RUNNING" || s.Desired != "awake" {
		t.Errorf("status = %+v, want RUNNING/awake", s)
	}
	if s.NextTransition == nil || s.NextTransition.To != "asleep" {
		t.Errorf("next transition = %+v, want asleep", s.NextTransition)
	}
	if !s.SSH.Available {
		t.Error("SSH should be available when running")
	}
}

func TestSchedule(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	mkSpan := func(start, end time.Time, st schedule.Desired) *schedule.Span {
		return &schedule.Span{Start: start, End: end, State: st}
	}
	// All times are in the future relative to the test now (2026-06-15 12:00 Stockholm).
	d15_2000 := time.Date(2026, 6, 15, 20, 0, 0, 0, loc)
	d15_2300 := time.Date(2026, 6, 15, 23, 0, 0, 0, loc)
	d16_0130 := time.Date(2026, 6, 16, 1, 30, 0, 0, loc)
	d16_0500 := time.Date(2026, 6, 16, 5, 0, 0, 0, loc)
	d16_0600 := time.Date(2026, 6, 16, 6, 0, 0, 0, loc)
	d16_0830 := time.Date(2026, 6, 16, 8, 30, 0, 0, loc)

	tests := []struct {
		name    string
		doc     *state.Document
		wantOut []string
	}{
		{
			name:    "no overrides",
			wantOut: []string{"Override:        none", "Hold:            none"},
		},
		{
			name:    "early wake — wakeEdge returns Start",
			doc:     &state.Document{Wake: mkSpan(d16_0500, d16_0600, schedule.Awake)},
			wantOut: []string{"Override:        wake", "05:00"},
		},
		{
			name:    "late wake — wakeEdge returns End",
			doc:     &state.Document{Wake: mkSpan(d16_0600, d16_0830, schedule.Asleep)},
			wantOut: []string{"Override:        wake", "08:30"},
		},
		{
			name:    "extend awake — sleepEdge returns End",
			doc:     &state.Document{Sleep: mkSpan(d15_2300, d16_0130, schedule.Awake)},
			wantOut: []string{"Override:        sleep", "01:30"},
		},
		{
			name:    "early sleep — sleepEdge returns Start",
			doc:     &state.Document{Sleep: mkSpan(d15_2000, d15_2300, schedule.Asleep)},
			wantOut: []string{"Override:        sleep", "20:00"},
		},
		{
			name:    "hold",
			doc:     &state.Document{Hold: mkSpan(time.Time{}, d15_2300, schedule.Awake)},
			wantOut: []string{"Hold:            stay awake until"},
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
}
