package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/state"
)

// capacityErr is the error a zone out of capacity produces, as the compute
// layer classifies it.
func capacityErr() *compute.CapacityError {
	return &compute.CapacityError{
		Code:           "ZONE_RESOURCE_POOL_EXHAUSTED",
		Zone:           "europe-north2-a",
		MachineType:    "e2-custom-4-8192",
		ZonesAvailable: []string{"europe-north2-b"},
		Message:        "A e2-custom-4-8192 VM instance is currently unavailable in the europe-north2-a zone.",
	}
}

// retryApp is testApp with a clock the retry loop can move: the injected wait
// advances it instead of the wall clock, so the tests run instantly and the
// give-up deadline is exact. clock.Now() reports the current fake time.
// The VM is always SUSPENDED: a capacity failure comes from the resume.
func retryApp(t *testing.T, now time.Time) (app *App, store *state.Fake, buf *bytes.Buffer, clock *testClock) {
	t.Helper()
	app, store, buf = testApp(t, compute.Suspended, now)
	app.Cfg.GCP.Zone = "europe-north2-a"
	app.Cfg.GCP.MachineType = "e2-custom-4-8192"
	clock = &testClock{now: now}
	app.Now = clock.Now
	app.wait = func(_ context.Context, d time.Duration) error {
		clock.advance(d)
		return nil
	}
	return app, store, buf, clock
}

// testClock is the one clock a retry test drives: the injected wait advances it,
// and a slowCompute can too, so fake time passes exactly where the production
// code would have waited.
type testClock struct {
	now time.Time
	// onAdvance runs before each step, for tests that assert on state while the
	// wake is still waiting.
	onAdvance func(time.Time)
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) advance(d time.Duration) {
	if c.onAdvance != nil {
		c.onAdvance(c.now)
	}
	c.now = c.now.Add(d)
}

// countingStore wraps state.Fake to count reads and writes and to fail every one
// after the first few, so a test can fail the hold top-ups without failing the
// write the wake itself depends on — state.Fake.SaveErr would fail that one too,
// before any wake happens. The top-up runs more than once (it is refreshed
// between attempts), so the hooks fail all later calls rather than a chosen one.
type countingStore struct {
	*state.Fake
	saves int
	loads int
	// failSaveAfter and failLoadAfter fail every call past that many; 0 never fails.
	failSaveAfter int
	failLoadAfter int
}

func (s *countingStore) Save(ctx context.Context, doc *state.Document) error {
	s.saves++
	if s.failSaveAfter > 0 && s.saves > s.failSaveAfter {
		return errors.New("writing state: deadline exceeded")
	}
	return s.Fake.Save(ctx, doc)
}

func (s *countingStore) Clear(ctx context.Context) error {
	s.saves++
	if s.failSaveAfter > 0 && s.saves > s.failSaveAfter {
		return errors.New("writing state: deadline exceeded")
	}
	return s.Fake.Clear(ctx)
}

func (s *countingStore) Load(ctx context.Context) (*state.Document, error) {
	s.loads++
	if s.failLoadAfter > 0 && s.loads > s.failLoadAfter {
		return nil, errors.New("reading state: deadline exceeded")
	}
	return s.Fake.Load(ctx)
}

// slowCompute makes a resume take real (fake) time, so a test can drive attempts
// that consume part of the retry window. Every test that installs one starts from
// SUSPENDED, so only the resume path needs wrapping; Suspend is left alone, since
// no capacity wait goes through it.
type slowCompute struct {
	*compute.Fake
	clock *testClock
	per   time.Duration
}

func (c *slowCompute) Resume(ctx context.Context) error {
	err := c.Fake.Resume(ctx)
	c.clock.advance(c.per)
	return err
}

// failingWrites fails every write while letting reads through, so a test can
// drive a post-wait pass whose save cannot land.
type failingWrites struct {
	*state.Fake
}

func (s *failingWrites) Save(context.Context, *state.Document) error {
	return errors.New("writing state: deadline exceeded")
}

func (s *failingWrites) Clear(context.Context) error {
	return errors.New("writing state: deadline exceeded")
}

// seedSleep stores a scheduled sleep for the next HH:MM, the arrangement several
// post-wait tests need before the wake runs.
func seedSleep(t *testing.T, app *App, now time.Time, hhmm string) {
	t.Helper()
	dt, err := schedule.ParseDayTime(hhmm)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := app.Store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Set(state.KindSleep, app.Sched.ScheduledSleep(now, dt))
	if err := app.Store.Save(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
}

// countResumes counts resume attempts; the retry loop makes one per attempt.
func countResumes(calls []string) int {
	n := 0
	for _, c := range calls {
		if c == "resume" {
			n++
		}
	}
	return n
}

// A zone that frees up within the window resumes the VM without the user
// having to run their own retry loop.
func TestWakeRetriesUntilCapacityReturns(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30) // outside working hours
	app, store, buf, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	if got := countResumes(fake.Calls); got != 3 {
		t.Errorf("resume calls = %d, want 3 (%v)", got, fake.Calls)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("VM state = %v, want Running", s)
	}
	if want := 2 * capacityRetryInterval; !clock.Now().Equal(now.Add(want)) {
		t.Errorf("elapsed = %v, want %v", clock.Now().Sub(now), want)
	}

	out := buf.String()
	first := "europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED) — retrying for up to about 5 minutes...\n"
	if strings.Count(out, first) != 1 {
		t.Errorf("wake output should explain the wait exactly once, got:\n%s", out)
	}
	if !strings.Contains(out, "Still no capacity in europe-north2-a (about 5 minutes left)...\n") {
		t.Errorf("wake output missing the per-attempt line, got:\n%s", out)
	}
	if !strings.Contains(out, "workbox is awake.\n") {
		t.Errorf("wake output missing the success line, got:\n%s", out)
	}

	// The grace was written before the resume, so the time spent retrying is
	// added back on.
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil {
		t.Fatal("expected a keep-awake grace hold")
	}
	wantEnd := clock.Now().Add(33 * time.Minute) // idle 30m + ssh wait 3m
	if !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("grace hold end = %v, want %v (extended from the post-wake clock)", doc.Hold.End, wantEnd)
	}
	if !strings.Contains(out, "Re-measured the keep-awake hold from now; it runs until Tue 2026-06-16 00:04 CEST (grace window), since the wake waited for capacity.\n") {
		t.Errorf("wake output missing the re-measured-hold line, got:\n%s", out)
	}
}

// A zone that stays full gives up at the window and explains the options,
// while the error itself stays a single line for main() to print.
func TestWakeCapacityGivesUpWithRemedies(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.Err = capacityErr()

	// Give the diagnosis its own stream so the split can be asserted.
	errBuf := &bytes.Buffer{}
	app.ErrOut = errBuf

	err := app.Wake(context.Background())
	if !errors.Is(err, compute.ErrNoCapacity) {
		t.Fatalf("Wake = %v, want a capacity error", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error must stay a single line, got %q", err.Error())
	}
	// Attempts at 0s, 30s, ..., 300s: the last one starts exactly on the
	// deadline, and none after it.
	wantAttempts := int(capacityRetryWindow/capacityRetryInterval) + 1
	if got := countResumes(fake.Calls); got != wantAttempts {
		t.Errorf("resume calls = %d, want %d (%v)", got, wantAttempts, fake.Calls)
	}
	if !clock.Now().Equal(now.Add(capacityRetryWindow)) {
		t.Errorf("gave up at %v, want %v", clock.Now(), now.Add(capacityRetryWindow))
	}

	// The whole capacity narrative — progress and conclusion alike — goes to
	// ErrOut, so it travels with the error main() prints and survives a stdout
	// the user redirected away.
	diag := errBuf.String()
	for _, want := range []string{
		"europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED) — retrying for up to about 5 minutes...\n",
		"Still no capacity in europe-north2-a (about 1 minute left)...\n",
		"No capacity for e2-custom-4-8192 in europe-north2-a after 11 attempts across 5 minutes.\n",
		"GCP says: A e2-custom-4-8192 VM instance is currently unavailable",
		"GCP reports capacity in: europe-north2-b\n",
		"The failed wake changed nothing about your disks, and the suspended\n",
		"`workbox status` says whether it is still SUSPENDED or now TERMINATED.\n",
		"the new shape must still support suspend (no GPUs, no Local SSD,\n",
		"See docs/operations.md, \"Zone has no capacity for the machine type\", for the full options.\n",
		"try again in a while; capacity shortages are usually transient\n",
		"change gcp.machine_type and run `make tf-apply`",
		"move to another zone — not just a config change",
	} {
		if !strings.Contains(diag, want) {
			t.Errorf("ErrOut missing %q\ngot:\n%s", want, diag)
		}
	}
	// Stdout carries results only: the hold line the command did write, and no
	// part of the failure.
	out := buf.String()
	for _, unwanted := range []string{"retrying for up to", "Still no capacity", "What you can do:", "workbox is awake."} {
		if strings.Contains(out, unwanted) {
			t.Errorf("Out has %q; the capacity report and a success claim both belong elsewhere, got:\n%s", unwanted, out)
		}
	}

	// The default grace (33m) already outlasts the retry window, so no mid-wait
	// refresh was needed and the hold on record is the one written before the
	// resume — still running when the wake gave up, which is what matters: a
	// late-succeeding operation can leave the VM RUNNING, and the reconciler
	// suspends a RUNNING VM whose hold has lapsed.
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil {
		t.Fatal("the keep-awake grace should survive a failed wake")
	}
	if wantEnd := now.Add(33 * time.Minute); !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("hold end = %v, want %v (the grace written before the resume)", doc.Hold.End, wantEnd)
	}
	if !doc.Hold.End.After(clock.Now()) {
		t.Errorf("hold ended %v, at or before the give-up moment %v", doc.Hold.End, clock.Now())
	}
}

// The refresh matters most on the path that gives up: with a short idle timeout
// the grace is shorter than the window plus a late-failing attempt, and a resume
// that lands after we stopped waiting leaves the VM RUNNING for a reconciler that
// suspends a RUNNING VM whose hold has lapsed.
func TestWakeCapacityGiveUpKeepsAShortGraceAlive(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, _, clock := retryApp(t, now)
	five := 5
	app.Cfg.Schedule.IdleTimeoutMinutes = &five
	fake := app.Compute.(*compute.Fake)
	fake.Err = capacityErr()

	if err := app.Wake(context.Background()); !errors.Is(err, compute.ErrNoCapacity) {
		t.Fatalf("Wake = %v, want a capacity error", err)
	}

	doc, _ := store.Load(context.Background())
	if doc.Hold == nil {
		t.Fatal("the keep-awake grace should survive a failed wake")
	}
	// The refresh stops once the hold reaches the cutoff (deadline + one window =
	// now+10m), so the last write is the first one to pass it: the attempt at
	// 2m30s, plus the 8m grace (idle 5m + ssh wait 3m) measured from there.
	wantEnd := now.Add(150 * time.Second).Add(8 * time.Minute)
	if !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("hold end = %v, want %v (the last mid-wait refresh)", doc.Hold.End, wantEnd)
	}
	// The two properties that make it worth writing: it outlives the give-up, and
	// it reaches past the 8 minutes the pre-wake line promised.
	if !doc.Hold.End.After(clock.Now()) {
		t.Errorf("hold ended %v, at or before the give-up moment %v", doc.Hold.End, clock.Now())
	}
	if !doc.Hold.End.After(now.Add(8 * time.Minute)) {
		t.Errorf("hold end = %v, want later than the pre-wake grace %v", doc.Hold.End, now.Add(8*time.Minute))
	}
}

// A 503 whose details never reached the operation proto classifies with nothing
// but a code, so the CLI fills the zone and machine type from config and leaves
// out the lines it has no server text for.
func TestWakeCapacityReportsBareErrorFromConfig(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, buf, _ := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.Err = &compute.CapacityError{Code: "ZONE_RESOURCE_POOL_EXHAUSTED"}

	err := app.Wake(context.Background())
	if !errors.Is(err, compute.ErrNoCapacity) {
		t.Fatalf("Wake = %v, want a capacity error", err)
	}
	out := buf.String()
	for _, want := range []string{
		"europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED) — retrying",
		"No capacity for e2-custom-4-8192 in europe-north2-a after 11 attempts",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	// The line main() prints, and the one docs/operations.md shows as the symptom.
	if want := "europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED)"; err.Error() != want {
		t.Errorf("returned error = %q, want %q", err.Error(), want)
	}
	for _, unwanted := range []string{"GCP says:", "GCP reports capacity in:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should omit %q when the server said nothing\ngot:\n%s", unwanted, out)
		}
	}
}

// The first notice quotes the time actually left, which a slow first attempt has
// already eaten into.
func TestWakeCapacityFirstNoticeQuotesTheTimeLeft(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, buf, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.Err = capacityErr()
	// 90s per attempt leaves 3m30s of the window when the first one fails. The
	// promise is floored, so it says three minutes — never more than the window
	// can honour.
	app.Compute = &slowCompute{Fake: fake, clock: clock, per: 90 * time.Second}

	if err := app.Wake(context.Background()); !errors.Is(err, compute.ErrNoCapacity) {
		t.Fatalf("Wake = %v, want a capacity error", err)
	}
	want := "retrying for up to about 3 minutes...\n"
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("first notice missing %q\ngot:\n%s", want, out)
	}
}

// A single failing attempt can outlast the whole window when the operation
// itself is slow: the loop must not start another one, and the report must read
// correctly for one attempt and a wait longer than the window.
func TestWakeCapacitySingleSlowAttempt(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, buf, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.Err = capacityErr()
	// Stand in for a resume that takes six minutes to fail.
	app.Compute = &slowCompute{Fake: fake, clock: clock, per: 6 * time.Minute}

	if err := app.Wake(context.Background()); !errors.Is(err, compute.ErrNoCapacity) {
		t.Fatalf("Wake = %v, want a capacity error", err)
	}
	if got := countResumes(fake.Calls); got != 1 {
		t.Errorf("resume calls = %d, want 1: no attempt may start past the deadline (%v)", got, fake.Calls)
	}
	if want := "No capacity for e2-custom-4-8192 in europe-north2-a after 1 attempt across 6 minutes.\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("output missing %q\ngot:\n%s", want, buf.String())
	}
}

// With idle shutdown off there is no hold to keep alive, so the retry runs with
// no top-up at all.
func TestWakeCapacityRetriesWithoutAHold(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, _ := retryApp(t, now)
	disableIdle(app)
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold != nil {
		t.Errorf("hold = %+v, want none with idle shutdown disabled", doc.Hold)
	}
	out := buf.String()
	if strings.Contains(out, "Re-measured the keep-awake hold") {
		t.Errorf("no hold exists to extend, got:\n%s", out)
	}
	if !strings.Contains(out, "workbox is awake.\n") {
		t.Errorf("wake output missing the success line, got:\n%s", out)
	}
}

// A failure that arrives after a capacity wait is still a failure: no hold
// extension is reported for a wake that did not succeed.
func TestWakeCapacityThenOtherFailureReportsNoExtension(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, buf, _ := retryApp(t, now)
	boom := errors.New("boom")
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), boom}

	if err := app.Wake(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Wake = %v, want %v", err, boom)
	}
	if out := buf.String(); strings.Contains(out, "Re-measured the keep-awake hold") {
		t.Errorf("a failed wake must not report an extension, got:\n%s", out)
	}
}

// sleepDuringWaitApp arranges the one scenario both post-wait sleep tests need:
// a wake at 23:58 with a sleep set for 00:00 — still in the future when the
// command starts, in effect by the time capacity returns five attempts later.
func sleepDuringWaitApp(t *testing.T) (*App, *state.Fake, *bytes.Buffer) {
	t.Helper()
	now := localTime(t, 6, 15, 23, 58)
	app, store, buf, _ := retryApp(t, now)
	seedSleep(t, app, now, "00:00")
	fake := app.Compute.(*compute.Fake)
	for range 5 {
		fake.ErrSeq = append(fake.ErrSeq, capacityErr())
	}
	fake.ErrSeq = append(fake.ErrSeq, nil)
	return app, store, buf
}

// A scheduled sleep that comes into effect while we wait outranks the hold, so
// the wake must cancel it rather than report an awake VM about to be suspended.
func TestWakeCancelsSleepThatBeginsDuringTheWait(t *testing.T) {
	app, store, buf := sleepDuringWaitApp(t)

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	stored, _ := store.Load(context.Background())
	if stored.Sleep != nil {
		t.Errorf("sleep = %+v, want it cancelled: it came into effect while waiting", stored.Sleep)
	}
	want := "Cancelled the scheduled sleep at Tue 2026-06-16 00:00 CEST: it came into effect while the wake waited for capacity.\n"
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("wake output missing %q\ngot:\n%s", want, out)
	}
}

// With no hold to keep it company, cancelling that sleep empties the document —
// which must then be deleted, since the reconciler only collects documents that
// still hold a span.
func TestWakeSleepCancellationClearsAnEmptiedDocument(t *testing.T) {
	app, store, _ := sleepDuringWaitApp(t)
	disableIdle(app) // no grace, so the cancelled sleep leaves nothing behind

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	if store.Doc != nil {
		t.Errorf("document = %+v, want it deleted once no span is left", store.Doc)
	}
}

// When the cancelling write fails the VM is up but a sleep in effect will suspend
// it, so the wake must still succeed and must name the recovery.
func TestWakeReportsSleepCancellationItCouldNotWrite(t *testing.T) {
	app, store, buf := sleepDuringWaitApp(t)
	disableIdle(app)
	// With idle shutdown off and the seeded sleep still in the future, the command
	// makes no pre-wake write at all, so every write the post-wait pass attempts
	// must fail — hence failingWrites rather than a countingStore that lets the
	// first one through.
	app.Store = &failingWrites{Fake: store}
	// The warning is diagnosis, so it belongs on ErrOut rather than a stdout the
	// user may have redirected away.
	errBuf := &bytes.Buffer{}
	app.ErrOut = errBuf

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil — the VM is up", err)
	}
	// The sleep is in effect, so the message gives a recovery rather than a
	// deadline; with idle shutdown off there is no hold, so that recovery is a
	// plain wake, which cancels an in-effect sleep and resumes in one command.
	want := "The scheduled sleep at Tue 2026-06-16 00:00 CEST is in effect and could not be cancelled (writing state: deadline exceeded); it may suspend the VM within the minute — run `workbox wake` to call it off and bring the VM back.\n"
	if diag := errBuf.String(); !strings.Contains(diag, want) {
		t.Errorf("ErrOut missing %q\ngot:\n%s", want, diag)
	}
	out := buf.String()
	if strings.Contains(out, "could not be cancelled") {
		t.Errorf("the warning belongs on ErrOut, not Out, got:\n%s", out)
	}
	if !strings.Contains(out, "workbox is awake.\n") {
		t.Errorf("wake output missing the success line, got:\n%s", out)
	}
}

// A capacity-failed resume can leave the VM TERMINATED (the memory image is
// discarded). The next attempt must start it rather than resume it.
func TestWakeRetryStartsTerminatedVM(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, _, _ := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.StatusSeq = []compute.State{compute.Suspended, compute.Terminated}
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	want := []string{"status", "resume", "status", "start"}
	if strings.Join(fake.Calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", fake.Calls, want)
	}
}

// Only capacity is retried: anything else is the user's problem to fix now.
func TestWakeDoesNotRetryOtherFailures(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "generic failure", err: errors.New("boom")},
		{name: "unsupported state", err: compute.ErrUnsupportedState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, buf, clock := retryApp(t, now)
			fake := app.Compute.(*compute.Fake)
			fake.Err = tc.err

			if err := app.Wake(context.Background()); !errors.Is(err, tc.err) {
				t.Fatalf("Wake = %v, want %v", err, tc.err)
			}
			if got := countResumes(fake.Calls); got != 1 {
				t.Errorf("resume calls = %d, want 1 (%v)", got, fake.Calls)
			}
			if !clock.Now().Equal(now) {
				t.Errorf("clock moved to %v; a non-capacity failure must not wait", clock.Now())
			}
			if out := buf.String(); strings.Contains(out, "no capacity") {
				t.Errorf("unexpected capacity message, got:\n%s", out)
			}
		})
	}
}

// Ctrl-C during a retry wait stops the wake instead of running out the window.
func TestWakeCapacityRetryHonorsCancellation(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, _, _ := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.Err = capacityErr()
	app.wait = func(_ context.Context, _ time.Duration) error { return context.Canceled }

	if err := app.Wake(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wake = %v, want context.Canceled", err)
	}
	if got := countResumes(fake.Calls); got != 1 {
		t.Errorf("resume calls = %d, want 1 (%v)", got, fake.Calls)
	}
}

// An already-cancelled context makes no compute call.
func TestWakeCapacityRetrySkipsCancelledContext(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, _, _ := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := app.Wake(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wake = %v, want context.Canceled", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %v, want none", fake.Calls)
	}
}

// The grace matters most with a short idle timeout, where retrying could
// otherwise consume all of it and let the reconciler suspend a booting VM.
func TestWakeExtendsShortGraceAfterRetrying(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, _, clock := retryApp(t, now)
	five := 5
	app.Cfg.Schedule.IdleTimeoutMinutes = &five
	refreshes := 0
	prev := now.Add(8 * time.Minute) // the grace written before the resume
	clock.onAdvance = func(time.Time) {
		doc, err := store.Load(context.Background())
		if err != nil || doc.Hold == nil {
			return
		}
		if doc.Hold.End.After(prev) {
			refreshes, prev = refreshes+1, doc.Hold.End
		}
	}
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	// The 8-minute grace outlasts the 5-minute deadline but not a wake that can
	// run past it, so the mid-wait refresh must have kept it alive rather than
	// skipping on the deadline alone.
	if refreshes < 1 {
		t.Error("the grace was never refreshed during the wait")
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil {
		t.Fatal("expected a keep-awake grace hold")
	}
	// idle 5m + ssh wait 3m, measured from the end of the retrying.
	wantEnd := clock.Now().Add(8 * time.Minute)
	if !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("grace hold end = %v, want %v", doc.Hold.End, wantEnd)
	}
	if !doc.Hold.End.After(now.Add(8 * time.Minute)) {
		t.Error("the grace was not extended past the pre-wake window")
	}
}

// `keep-awake 3h` promises three hours of awake VM, so a capacity wait is not
// taken out of the hold: it is re-measured once the VM is up.
func TestKeepAwakeRetriesCapacity(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	if got := countResumes(fake.Calls); got != 2 {
		t.Errorf("resume calls = %d, want 2 (%v)", got, fake.Calls)
	}
	doc, _ := store.Load(context.Background())
	wantEnd := clock.Now().Add(3 * time.Hour)
	if doc.Hold == nil || !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("hold = %+v, want the requested 3h from the post-wake clock (%v)", doc.Hold, wantEnd)
	}
	out := buf.String()
	if !strings.Contains(out, "Re-measured the keep-awake hold from now; it runs until ") {
		t.Errorf("keep-awake output missing the re-measured-hold line, got:\n%s", out)
	}
	if !strings.Contains(out, "workbox is awake.\n") {
		t.Errorf("keep-awake output missing the success line, got:\n%s", out)
	}
}

// The hold is kept alive *while* we wait, not only repaired afterwards: the VM
// can come up on any attempt, and the reconciler suspends a RUNNING VM whose
// hold has lapsed.
func TestKeepAwakeKeepsHoldAliveWhileWaiting(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, _, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	for range 4 {
		fake.ErrSeq = append(fake.ErrSeq, capacityErr())
	}
	fake.ErrSeq = append(fake.ErrSeq, nil)
	// A 40-second hold: without the per-attempt refresh it lapses during the
	// second wait, well before the VM comes up on the fifth attempt.
	checks := 0
	clock.onAdvance = func(at time.Time) {
		checks++
		doc, err := store.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if doc.Hold == nil || !doc.Hold.End.After(at) {
			t.Fatalf("hold %+v had lapsed at %v, while the wake was still waiting", doc.Hold, at)
		}
	}

	if err := app.KeepAwake(context.Background(), 40*time.Second); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	if checks < 4 {
		t.Errorf("checked the hold %d times, want one per wait", checks)
	}
}

// A mid-wait refresh must not quietly delete a scheduled sleep: only the
// reported extension cancels one, so the user always hears about it.
func TestKeepAwakeRefreshDoesNotSilentlyCancelSleep(t *testing.T) {
	now := localTime(t, 6, 15, 18, 0)
	app, store, buf, _ := retryApp(t, now)
	// A sleep just past the requested hold (18:02, and the reach test is strict,
	// so the pre-wake write preserves it). A mid-wait refresh at 18:00:30 would
	// reach past it; the reported extension at 18:01 does, and says so.
	seedSleep(t, app, now, "18:02")
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), capacityErr(), nil}

	if err := app.KeepAwake(context.Background(), 2*time.Minute); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	stored, _ := store.Load(context.Background())
	out := buf.String()
	// The pre-wake write must have preserved the sleep, or the test would pass
	// for the wrong reason.
	if !strings.Contains(out, "Scheduled sleep still set for ") {
		t.Fatalf("the sleep was cancelled before the wait, so this pins nothing\ngot:\n%s", out)
	}
	if stored.Sleep != nil {
		t.Errorf("sleep = %+v, want it cancelled by the re-measured hold", stored.Sleep)
	}
	cancel := strings.Index(out, "Cancelled the scheduled sleep at ")
	if cancel < 0 {
		t.Fatalf("the scheduled sleep was cancelled without saying so, got:\n%s", out)
	}
	// The extension is what reached past the sleep, so it is reported first.
	if extend := strings.Index(out, "Re-measured the keep-awake hold from now; it runs until "); extend < 0 || extend > cancel {
		t.Errorf("the cancellation should follow the extension that caused it, got:\n%s", out)
	}
}

// The qualifier a hold was first reported with travels with it, so the extension
// line does not read as a different hold.
func TestKeepAwakeExtensionKeepsTheReportedQualifier(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, buf, _ := retryApp(t, now)
	disableIdle(app)
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	want := "Re-measured the keep-awake hold from now; it runs until Tue 2026-06-16 02:30 CEST (idle shutdown is disabled, so nothing would auto-suspend anyway), since the wake waited for capacity.\n"
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("keep-awake output missing %q\ngot:\n%s", want, out)
	}
}

// A hold short enough to be eaten by the capacity wait is exactly the case the
// re-measurement exists for: without it the VM comes up with an expired hold.
func TestKeepAwakeShortHoldSurvivesCapacityWait(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, _, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), capacityErr(), nil}

	if err := app.KeepAwake(context.Background(), 40*time.Second); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil {
		t.Fatal("expected a keep-awake hold")
	}
	if !doc.Hold.End.After(clock.Now()) {
		t.Errorf("hold ends %v, which is not after the post-wake clock %v", doc.Hold.End, clock.Now())
	}
}

// A wake that preserved a longer hold of the user's own still gets that hold
// extended when the capacity wait leaves it shorter than the boot window needs.
func TestWakeExtendsPreservedHoldAfterWaiting(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, clock := retryApp(t, now)
	five := 5
	app.Cfg.Schedule.IdleTimeoutMinutes = &five // grace is 5m + 3m ssh wait
	// A user hold ending just past the pre-wake grace: kept as-is by Wake, but
	// too short once five minutes go on waiting for capacity.
	existing := app.Sched.KeepAwakeHold(now, 9*time.Minute)
	doc := &state.Document{}
	doc.Set(state.KindHold, existing)
	if err := app.Store.Save(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	// Fail for the whole window (attempts at 0s, 30s, ... 270s), then succeed on
	// the attempt that starts exactly at the deadline.
	fake := app.Compute.(*compute.Fake)
	for range int(capacityRetryWindow / capacityRetryInterval) {
		fake.ErrSeq = append(fake.ErrSeq, capacityErr())
	}
	fake.ErrSeq = append(fake.ErrSeq, nil)

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	stored, _ := store.Load(context.Background())
	if stored.Hold == nil {
		t.Fatal("expected a keep-awake hold")
	}
	wantEnd := clock.Now().Add(8 * time.Minute)
	if !stored.Hold.End.Equal(wantEnd) {
		t.Errorf("hold end = %v, want %v (extended past the preserved hold)", stored.Hold.End, wantEnd)
	}
	out := buf.String()
	if !strings.Contains(out, "A longer keep-awake hold is already in place") {
		t.Errorf("wake output should report the preserved hold, got:\n%s", out)
	}
	// What the top-up writes is the grace, whatever hold it replaced, so it is
	// labelled as one even though the preserved hold was not.
	if !strings.Contains(out, "Re-measured the keep-awake hold from now; it runs until Mon 2026-06-15 23:43 CEST (grace window)") {
		t.Errorf("wake output missing the grace-window qualifier, got:\n%s", out)
	}
}

// The extension is a top-up, not the operation the user asked for: a failed
// write must not turn a successful wake into a failed command, which in the
// ssh/herdr flows would cost the user the session on a running VM.
func TestWakeReportsButSurvivesFailedHoldExtension(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, _ := retryApp(t, now)
	// Fail the top-up's writes; the pre-wake write must land.
	app.Store = &countingStore{Fake: store, failSaveAfter: 1}
	errBuf := &bytes.Buffer{}
	app.ErrOut = errBuf
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil — the VM is up", err)
	}
	out := buf.String()
	if strings.Contains(out, "Could not extend the keep-awake hold") {
		t.Errorf("the warning belongs on ErrOut, not Out, got:\n%s", out)
	}
	// The wake path's recovery is `workbox wake`: re-running keep-awake would
	// cancel a future scheduled sleep this path preserves.
	if !strings.Contains(errBuf.String(), "Could not extend the keep-awake hold (writing state: deadline exceeded); it runs at least as long as reported above — run `workbox wake` to set it from now.\n") {
		t.Errorf("ErrOut missing the extension warning, got:\n%s", errBuf.String())
	}
	if !strings.Contains(out, "workbox is awake.\n") {
		t.Errorf("wake output missing the success line, got:\n%s", out)
	}
	// The grace written before the resume is still on record.
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil || !doc.Hold.End.Equal(now.Add(33*time.Minute)) {
		t.Errorf("hold = %+v, want the pre-wake grace intact", doc.Hold)
	}
}

// The same holds when the extension cannot even read the current state: the
// wake succeeded, so the command must not fail on a top-up it skipped.
func TestWakeReportsButSurvivesFailedHoldRead(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, _ := retryApp(t, now)
	// Wake's own read is the first; every top-up read after it fails.
	app.Store = &countingStore{Fake: store, failLoadAfter: 1}
	errBuf := &bytes.Buffer{}
	app.ErrOut = errBuf
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil — the VM is up", err)
	}
	out := buf.String()
	if strings.Contains(out, "Could not re-check") {
		t.Errorf("the warning belongs on ErrOut, not Out, got:\n%s", out)
	}
	if !strings.Contains(errBuf.String(), "Could not re-check the keep-awake hold and scheduled sleep (reading state: deadline exceeded); run `workbox schedule` to see where they stand.\n") {
		t.Errorf("ErrOut missing the re-check warning, got:\n%s", errBuf.String())
	}
	if !strings.Contains(out, "workbox is awake.\n") {
		t.Errorf("wake output missing the success line, got:\n%s", out)
	}
}

// The extension never shortens a hold: a long keep-awake of the user's own
// outlasts the boot grace, so a capacity wait must leave it exactly as it was.
func TestWakeHoldExtensionNeverShortensALongHold(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, _ := retryApp(t, now)
	existing := app.Sched.KeepAwakeHold(now, 8*time.Hour)
	doc := &state.Document{}
	doc.Set(state.KindHold, existing)
	if err := app.Store.Save(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	// Count writes from here: the preserved hold means the pre-wake path has
	// nothing to write either, so a post-wait pass that changes nothing leaves
	// the document untouched altogether.
	counting := &countingStore{Fake: store}
	app.Store = counting
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	if counting.saves != 0 {
		t.Errorf("state saves = %d, want 0: nothing changed, so nothing is written", counting.saves)
	}
	stored, _ := store.Load(context.Background())
	if stored.Hold == nil || !stored.Hold.End.Equal(existing.End) {
		t.Errorf("hold = %+v, want the 8h hold untouched (ends %v)", stored.Hold, existing.End)
	}
	if out := buf.String(); strings.Contains(out, "Re-measured the keep-awake hold") {
		t.Errorf("a hold longer than the grace must not be reported as extended, got:\n%s", out)
	}
}

// A `workbox cancel` run from another shell during the wait must stay
// cancelled: the extension reads current state instead of writing back the
// snapshot it loaded before the wait.
func TestWakeHoldExtensionDoesNotResurrectCancelledState(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, _, _ := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}
	// Stand in for the concurrent `workbox cancel`: clear the store while the
	// wake is waiting for capacity.
	app.wait = func(_ context.Context, _ time.Duration) error {
		return store.Clear(context.Background())
	}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold != nil {
		t.Errorf("hold = %+v, want none: the cancel during the wait stands", doc.Hold)
	}
	if doc.Sleep != nil {
		t.Errorf("sleep = %+v, want none", doc.Sleep)
	}
}

// A wake that never had to wait does not write the hold a second time.
func TestWakeWithoutCapacityWaitWritesHoldOnce(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, _, _ := retryApp(t, now)
	counting := &countingStore{Fake: store}
	app.Store = counting

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	if counting.saves != 1 {
		t.Errorf("state saves = %d, want 1 (the grace written before the resume)", counting.saves)
	}
}

// The two duration renderings differ on purpose: the per-attempt lines round up,
// so seconds left never read as none, while the opening promise and the give-up
// line count only whole minutes that passed — with minutesText's one-minute floor
// as the single exception, which the first row below pins.
func TestDurationAndAttemptWording(t *testing.T) {
	for _, tc := range []struct {
		d                  time.Duration
		remaining, elapsed string
	}{
		{d: 30 * time.Second, remaining: "1 minute", elapsed: "1 minute"},
		{d: 4*time.Minute + 31*time.Second, remaining: "5 minutes", elapsed: "4 minutes"},
		{d: 5 * time.Minute, remaining: "5 minutes", elapsed: "5 minutes"},
		{d: 5*time.Minute + 40*time.Second, remaining: "6 minutes", elapsed: "5 minutes"},
	} {
		if got := atLeastMinutes(tc.d); got != tc.remaining {
			t.Errorf("atLeastMinutes(%v) = %q, want %q", tc.d, got, tc.remaining)
		}
		if got := atMostMinutes(tc.d); got != tc.elapsed {
			t.Errorf("atMostMinutes(%v) = %q, want %q", tc.d, got, tc.elapsed)
		}
	}
	for n, want := range map[int]string{1: "1 attempt", 2: "2 attempts", 11: "11 attempts"} {
		if got := attemptsText(n); got != want {
			t.Errorf("attemptsText(%d) = %q, want %q", n, got, want)
		}
	}
}

// The recovery hint names a duration the user can type, so it has to parse and
// to match the hold it replaces — a grace of idle plus a 45-second SSH wait is
// not a whole number of minutes.
func TestShortDurationRendersATypeableDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour:                   "2h",
		90 * time.Minute:                "90m",
		30*time.Minute + 45*time.Second: "30m45s",
		40 * time.Second:                "40s",
	} {
		got := shortDuration(d)
		if got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
		parsed, err := time.ParseDuration(got)
		if err != nil {
			t.Errorf("shortDuration(%v) = %q, which time.ParseDuration rejects: %v", d, got, err)
		} else if parsed != d {
			t.Errorf("shortDuration(%v) = %q, which parses back as %v", d, got, parsed)
		}
	}
}

// The last wait is trimmed to what is left of the window, so no attempt starts
// after the deadline even when the attempts themselves consume part of it. (The
// attempt already running when the deadline passes is never cut short.)
func TestWakeCapacityTrimsTheFinalWait(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, buf, clock := retryApp(t, now)
	fake := app.Compute.(*compute.Fake)
	fake.Err = capacityErr()
	// 31s per attempt leaves 25s of the window before the sixth one, so the
	// last wait must be trimmed from 30s to 25s; untrimmed, that attempt would
	// start 5s past the deadline.
	const per = 31 * time.Second
	app.Compute = &slowCompute{Fake: fake, clock: clock, per: per}

	if err := app.Wake(context.Background()); !errors.Is(err, compute.ErrNoCapacity) {
		t.Fatalf("Wake = %v, want a capacity error", err)
	}
	// The final attempt started exactly on the deadline and then took its own
	// time to fail.
	if want := now.Add(capacityRetryWindow + per); !clock.Now().Equal(want) {
		t.Errorf("gave up at %v, want %v: the final wait was not trimmed to the window", clock.Now(), want)
	}
	if want := "after 6 attempts across 5 minutes"; !strings.Contains(buf.String(), want) {
		t.Errorf("output missing %q\ngot:\n%s", want, buf.String())
	}
}

// The retry window and the section the give-up report points at are contracts
// with the docs, which the code comments say to keep in step. Pin them, as the
// repo pins its other cross-file contracts.
func TestCapacityDocsMatchTheCode(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const (
		readme = "../../README.md"
		ops    = "../../docs/operations.md"
		arch   = "../../docs/architecture.md"
	)
	docs := map[string]string{readme: read(readme), ops: read(ops), arch: read(arch)}
	// The prose wraps, so compare the multi-line claims against a
	// whitespace-normalized copy.
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	window := "about " + atMostMinutes(capacityRetryWindow)
	for _, path := range []string{readme, ops} {
		if !strings.Contains(docs[path], window) {
			t.Errorf("%s does not quote the retry window as %q; update it with capacityRetryWindow", path, window)
		}
	}

	if !strings.Contains(docs[ops], "\n### "+capacitySection+"\n") {
		t.Errorf("docs/operations.md has no heading that is exactly %q; the give-up report quotes it", capacitySection)
	}

	// The links that jump to that heading are part of the same contract: a
	// rename has to move them too, or they become dead anchors. architecture.md
	// links to the section as well, though it deliberately does not quote the
	// window, so it is absent from the phrase loop above.
	anchor := "#" + strings.ReplaceAll(strings.ToLower(capacitySection), " ", "-")
	for _, path := range []string{readme, ops, arch} {
		// With the closing delimiter, so a renamed section cannot leave the
		// links pointing at a prefix of the old anchor.
		if !strings.Contains(docs[path], anchor+")") {
			t.Errorf("%s links nowhere to %q; the section it points at is named by capacitySection", path, anchor)
		}
	}

	// The docs quote two strings the code produces. Derive them here, so
	// rewording the code fails this test instead of stranding the prose.
	buf := &bytes.Buffer{}
	(&App{Out: buf}).capacityRemedy(capacityErr(), 1, capacityRetryWindow)
	ceiling := "no GPUs, no Local SSD, at most 208 GB of memory"
	if !strings.Contains(flat(buf.String()), ceiling) {
		t.Errorf("the remedy block no longer states %q, which docs/operations.md quotes", ceiling)
	}
	if !strings.Contains(flat(docs[ops]), ceiling) {
		t.Errorf("docs/operations.md no longer states %q, which the remedy block also prints", ceiling)
	}

	// The symptom the section is keyed on is main()'s "workbox: %s" around
	// CapacityError.Error(); a reader greps the docs for the line they got.
	symptom := "workbox: " + (&compute.CapacityError{
		Code:        "ZONE_RESOURCE_POOL_EXHAUSTED",
		Zone:        "europe-north2-a",
		MachineType: "e2-custom-8-16384",
	}).Error()
	if !strings.Contains(docs[ops], symptom) {
		t.Errorf("docs/operations.md does not show %q as the symptom; it must match what the binary prints", symptom)
	}
}

// A plain wake must leave a merely-future scheduled sleep alone: only keep-awake
// cancels one its requested hold reaches over, and the grace never does.
func TestWakeLeavesAFutureSleepAlone(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, _ := retryApp(t, now)
	// 23:50 is inside the 33-minute grace the wake re-measures, but still in the
	// future when the VM comes up — nothing should touch it.
	seedSleep(t, app, now, "23:50")
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	stored, _ := store.Load(context.Background())
	if stored.Sleep == nil {
		t.Error("the future scheduled sleep was cancelled; only keep-awake may do that")
	}
	if out := buf.String(); strings.Contains(out, "Cancelled the scheduled sleep") {
		t.Errorf("wake must not report cancelling a future sleep, got:\n%s", out)
	}
}

// The hold report must not claim nothing would auto-suspend while a scheduled
// sleep that will is on record.
func TestKeepAwakeQualifierSeesASurvivingSleep(t *testing.T) {
	now := localTime(t, 6, 15, 18, 0)
	app, _, buf, _ := retryApp(t, now)
	disableIdle(app)
	// 20:00 is well past the two minutes requested, so it survives both the
	// pre-wake rule and the re-measured hold.
	seedSleep(t, app, now, "20:00")
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.KeepAwake(context.Background(), 2*time.Minute); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Re-measured the keep-awake hold from now; it runs until ") {
		t.Fatalf("output missing the extension line, got:\n%s", out)
	}
	if strings.Contains(out, "idle shutdown is disabled, so nothing would auto-suspend anyway") {
		t.Errorf("the qualifier must not survive a scheduled sleep that will suspend the VM, got:\n%s", out)
	}
}

// A sleep the re-measured hold reaches over but which is not yet in effect: the
// counterpart of the in-effect branch pinned below, with the same single-command
// recovery.
func TestKeepAwakeReportsFutureSleepCancellationItCouldNotWrite(t *testing.T) {
	now := localTime(t, 6, 15, 18, 0)
	app, store, buf, _ := retryApp(t, now)
	// 18:02 is exactly the requested hold's end, and the pre-wake reach-over test
	// is strict, so that pass leaves the sleep alone; only the 30-second wait
	// pushes the re-measured hold past it.
	seedSleep(t, app, now, "18:02")
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}
	// The hold KeepAwake writes before waking is save #1; the post-wait write fails.
	app.Store = &countingStore{Fake: store, failSaveAfter: 1}
	errBuf := &bytes.Buffer{}
	app.ErrOut = errBuf

	if err := app.KeepAwake(context.Background(), 2*time.Minute); err != nil {
		t.Fatalf("KeepAwake = %v, want nil — the VM is up", err)
	}
	if out := buf.String(); strings.Contains(out, "could not be cancelled") {
		t.Errorf("the warning belongs on ErrOut, not Out, got:\n%s", out)
	}
	diag := errBuf.String()
	want := "The scheduled sleep at Mon 2026-06-15 18:02 CEST could not be cancelled (writing state: deadline exceeded), though the re-measured keep-awake hold now runs past it; run `workbox keep-awake 2m` to call it off before it suspends the VM.\n"
	if !strings.Contains(diag, want) {
		t.Errorf("ErrOut missing %q\ngot:\n%s", want, diag)
	}
	// Only one half is reported, so the hold message must not also appear.
	if strings.Contains(diag, "Could not extend the keep-awake hold") {
		t.Errorf("both halves reported, got:\n%s", diag)
	}
}

// With the sleep cancelled there is nothing left to auto-suspend, so the
// qualifier returns — the mirror of TestKeepAwakeQualifierSeesASurvivingSleep.
func TestKeepAwakeQualifierAfterCancellingTheSleep(t *testing.T) {
	app, _, buf := sleepDuringWaitApp(t)
	disableIdle(app)

	if err := app.KeepAwake(context.Background(), time.Minute); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Cancelled the scheduled sleep at ") {
		t.Fatalf("the in-effect sleep should have been cancelled, got:\n%s", out)
	}
	if !strings.Contains(out, " (idle shutdown is disabled, so nothing would auto-suspend anyway),") {
		t.Errorf("the qualifier should return once no sleep survives, got:\n%s", out)
	}
}

// A keep-awake whose write fails names the duration it asked for; the wake path
// names `workbox wake`, which re-measures its own grace.
func TestKeepAwakeReportsSleepCancellationItCouldNotWrite(t *testing.T) {
	app, store, buf := sleepDuringWaitApp(t)
	// The hold KeepAwake writes before waking is save #1; everything the
	// post-wait pass writes fails.
	app.Store = &countingStore{Fake: store, failSaveAfter: 1}
	errBuf := &bytes.Buffer{}
	app.ErrOut = errBuf

	// One minute: short enough that the hold written before the wake does not
	// reach the 00:00 sleep, so the sleep survives to come into effect during
	// the wait and the post-wait pass is what tries to cancel it.
	if err := app.KeepAwake(context.Background(), time.Minute); err != nil {
		t.Fatalf("KeepAwake = %v, want nil — the VM is up", err)
	}
	// The sleep is in effect by now, so this is the recovery wording; the hint
	// names the duration keep-awake asked for.
	want := "is in effect and could not be cancelled (writing state: deadline exceeded); it may suspend the VM within the minute — run `workbox keep-awake 1m` to call it off and bring the VM back.\n"
	if diag := errBuf.String(); !strings.Contains(diag, want) {
		t.Errorf("ErrOut missing the recovery hint %q\ngot:\n%s", want, diag)
	}
	if out := buf.String(); strings.Contains(out, "could not be cancelled") {
		t.Errorf("the warning belongs on ErrOut, not Out, got:\n%s", out)
	}
}

// Keep-awake's sleep rule follows the hold it asked for, not the one this pass
// wrote: re-measured from the post-wake clock the requested hold reaches over a
// sleep it missed before the wait, and a longer pre-existing hold — which
// suppresses the write entirely — must not suppress that cancellation.
func TestKeepAwakeCancelsSleepUnderAPreservedLongerHold(t *testing.T) {
	now := localTime(t, 6, 15, 18, 0)
	app, store, buf, _ := retryApp(t, now)
	dt, err := schedule.ParseDayTime("18:02")
	if err != nil {
		t.Fatal(err)
	}
	doc := &state.Document{}
	doc.Set(state.KindSleep, app.Sched.ScheduledSleep(now, dt))
	// A hold far longer than the two minutes requested below, so the top-up
	// writes nothing.
	doc.Set(state.KindHold, app.Sched.KeepAwakeHold(now, 6*time.Hour))
	if err := app.Store.Save(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}

	if err := app.KeepAwake(context.Background(), 2*time.Minute); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	stored, _ := store.Load(context.Background())
	if stored.Sleep != nil {
		t.Errorf("sleep = %+v, want it cancelled by the requested hold", stored.Sleep)
	}
	if out := buf.String(); !strings.Contains(out, "Cancelled the scheduled sleep at ") {
		t.Errorf("output missing the cancellation, got:\n%s", out)
	}
}

// The qualifier is derived from the sleep the pruned view keeps: a span the
// reconciler ignores must not make the report claim something would suspend the
// VM.
func TestKeepAwakeQualifierIgnoresAPrunedSleep(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, _, buf, clock := retryApp(t, now)
	disableIdle(app)
	fake := app.Compute.(*compute.Fake)
	fake.ErrSeq = []error{capacityErr(), nil}
	// Land the span while the wake waits, so the pruning in the post-wait pass
	// is what has to drop it: an awake-stated span in the sleep slot is
	// malformed, and nothing — reconciler included — treats it as a sleep.
	store := app.Store
	clock.onAdvance = func(time.Time) {
		doc, err := store.Load(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		doc.Set(state.KindSleep, app.Sched.KeepAwakeHold(now, time.Hour))
		if err := store.Save(context.Background(), doc); err != nil {
			t.Error(err)
		}
	}

	if err := app.KeepAwake(context.Background(), 3*time.Hour); err != nil {
		t.Fatalf("KeepAwake = %v, want nil", err)
	}
	// The qualifier stands: the malformed span is not something that would
	// auto-suspend the VM.
	want := " (idle shutdown is disabled, so nothing would auto-suspend anyway), since the wake waited for capacity.\n"
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
	if out := buf.String(); strings.Contains(out, "Cancelled the scheduled sleep") {
		t.Errorf("a span the pruned view drops must not be cancelled, got:\n%s", out)
	}
}

// The production wait — the one the retry loop uses when no fake clock is
// injected — must return on cancellation rather than sitting out the interval.
func TestWaitForHonorsCancellationAndElapses(t *testing.T) {
	app, _, _, _ := retryApp(t, localTime(t, 6, 15, 23, 30))
	app.wait = nil // exercise the real timer

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.waitFor(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("waitFor on a cancelled context = %v, want context.Canceled", err)
	}
	if err := app.waitFor(context.Background(), time.Millisecond); err != nil {
		t.Errorf("waitFor = %v, want nil once the timer fires", err)
	}
}

// An ordinary wake takes time too — the resume itself is not instant — but the
// grace is sized for that, so nothing is rewritten and nothing is printed.
func TestWakeWithoutCapacityWaitLeavesGraceAlone(t *testing.T) {
	now := localTime(t, 6, 15, 23, 30)
	app, store, buf, _ := retryApp(t, now)
	// A clock that advances on every reading, as the wall clock does during a
	// resume that takes a while.
	cur := now
	app.Now = func() time.Time {
		cur = cur.Add(20 * time.Second)
		return cur
	}

	if err := app.Wake(context.Background()); err != nil {
		t.Fatalf("Wake = %v, want nil", err)
	}
	if out := buf.String(); strings.Contains(out, "Re-measured the keep-awake hold") {
		t.Errorf("a wake that never waited for capacity must not extend the hold, got:\n%s", out)
	}
	doc, _ := store.Load(context.Background())
	if doc.Hold == nil {
		t.Fatal("expected a keep-awake grace hold")
	}
	// Still exactly the grace written before the resume, so a top-up that stopped
	// printing could not pass for one that never ran. The clock advances 20s per
	// reading, and the pre-wake write is measured from the first one.
	if wantEnd := now.Add(20 * time.Second).Add(33 * time.Minute); !doc.Hold.End.Equal(wantEnd) {
		t.Errorf("hold end = %v, want %v (the grace written before the resume, untouched)", doc.Hold.End, wantEnd)
	}
}
