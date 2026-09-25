// Package cli holds the state-affecting command logic for workbox, operating on
// the compute.Compute and state.Store interfaces and an injectable clock so the
// commands are unit-testable without real cloud resources. Connectivity flows
// (ssh/herdr handoff) live in cmd/workbox because they replace the process.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/state"
)

// App carries the collaborators the commands need.
type App struct {
	Cfg     *config.Config
	Sched   schedule.Schedule
	Compute compute.Compute
	Store   state.Store
	// Activity reports when the VM was last active; nil omits the last-active
	// readout, and a verdict that depends on activity is then reported as unknown.
	Activity compute.Activity
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
	Out io.Writer
	// ErrOut takes failure diagnosis, so advice travels with the error main()
	// prints rather than down a stream the user may have redirected away.
	// Defaults to Out, which keeps a test's single buffer intact.
	ErrOut io.Writer
	// wait pauses between wake retries; nil uses a real timer. Unexported
	// because it exists for tests, which drive it with a fake clock. Not named
	// sleep: that is the command that suspends the VM.
	wait func(ctx context.Context, d time.Duration) error
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Out, format, args...)
}

// errPrintf writes failure diagnosis to ErrOut.
func (a *App) errPrintf(format string, args ...any) {
	out := a.ErrOut
	if out == nil {
		out = a.Out
	}
	fmt.Fprintf(out, format, args...)
}

// graceWindow is how long a freshly woken VM is held awake: long enough to
// outlast the boot and the wait for SSH before idle shutdown can apply. Zero when
// idle shutdown is disabled, since the grace only guards against that.
func (a *App) graceWindow() time.Duration {
	idle := a.Cfg.Schedule.IdleTimeout()
	if idle <= 0 {
		return 0
	}
	return idle + a.Cfg.SSH.WaitTimeout()
}

// idleDisabledNote qualifies a hold report when nothing else would suspend the
// VM anyway: idle shutdown off, and no scheduled sleep to outrank the hold. A
// surviving sleep is reported separately.
func (a *App) idleDisabledNote(sleep *schedule.Span) string {
	if a.Cfg.Schedule.IdleTimeout() <= 0 && sleep == nil {
		return " (idle shutdown is disabled, so nothing would auto-suspend anyway)"
	}
	return ""
}

// saveOrClear writes doc, deleting it instead when no spans are left: the
// reconciler only garbage-collects documents that still hold one, so an empty
// document would linger forever.
func (a *App) saveOrClear(ctx context.Context, doc *state.Document) error {
	if doc.Empty() {
		return a.Store.Clear(ctx)
	}
	return a.Store.Save(ctx, doc)
}

// activeDoc loads the state document with expired spans pruned as of now.
func (a *App) activeDoc(ctx context.Context, now time.Time) (*state.Document, error) {
	doc, err := a.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	return doc.Active(now), nil
}

// Wake resumes/starts the VM now and establishes a short keep-awake grace hold so
// the reconciler does not suspend it before activity is first reported. The grace
// is the idle timeout plus the SSH wait timeout; it is skipped when idle shutdown
// is disabled, since the grace only guards against idle shutdown (a scheduled
// sleep wins over a hold regardless). A scheduled sleep that is currently in
// effect is cancelled — a future one is left in place — so the reconciler does
// not immediately re-suspend the VM. The wake is retried for a bounded window when
// the zone cannot place the machine type (see wakeWithRetry), so this can block
// for some minutes; a wake that waited re-measures the grace from the post-wake
// clock, so the wait is not taken out of it, and re-applies the in-effect test — a
// sleep that began while we waited is cancelled too, since it would otherwise
// suspend the VM this call just reported awake.
func (a *App) Wake(ctx context.Context) error {
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	changed := false
	// A manual wake cancels a scheduled sleep that is currently in effect, so the
	// reconciler does not re-suspend the VM you just woke. A future scheduled sleep
	// is left in place.
	var cancelled *schedule.Span
	if doc.Sleep.Active(now) {
		cancelled, doc.Sleep = doc.Sleep, nil
		changed = true
	}
	var graceMsg string
	var top *holdTopUp
	if window := a.graceWindow(); window > 0 {
		// Written before the resume, so a VM that comes up is protected from the
		// first reconciler tick onwards.
		grace := a.Sched.KeepAwakeHold(now, window)
		hold, kept := longerAwakeHold(doc, grace)
		note := "" // a preserved hold is the user's own, not a grace window
		if !kept {
			doc.Set(state.KindHold, hold)
			changed = true
			note = " (grace window)"
		}
		graceMsg = holdMessage(a.fmtTime(hold.End), kept, note)
		// The same window, re-measured after a capacity wait: settleAfterWait
		// never shortens a hold, so a preserved one is only touched when the
		// fresh grace would outlast it — and what it writes is then the grace
		// window, whatever hold it replaced, so it is always labelled as one.
		top = &holdTopUp{d: window, grace: true}
	}
	// Persist before the compute call so a failed resume still leaves the grace in
	// place (and the cancelled sleep gone) for the next reconciler tick.
	if changed {
		if err := a.saveOrClear(ctx, doc); err != nil {
			return err
		}
	}
	a.printCancelledSleep(cancelled)
	if graceMsg != "" {
		a.printf("%s", graceMsg)
	}
	a.printPendingSleep(doc.Sleep)
	// The hold above was measured before the resume, so wakeWithRetry keeps it
	// alive while it waits and reports the end the user should count on.
	if err := a.wakeWithRetry(ctx, top); err != nil {
		return err
	}
	a.printf("workbox is awake.\n")
	return nil
}

// Sleep suspends the VM now and clears any keep-awake hold or scheduled sleep, so
// the machine stays asleep until a manual wake. The reconciler never resumes, so
// no hold is needed to make a manual sleep stick.
func (a *App) Sleep(ctx context.Context) error {
	// Suspend first, so a Firestore failure can't leave a billable VM running.
	if err := compute.Sleep(ctx, a.Compute); err != nil {
		return err
	}
	// Clear once the suspend landed — a failed suspend leaves the hold intact
	// rather than exposing the VM to the next reconciler tick — and clear even
	// if the read failed: the document is only used for the report below.
	doc, loadErr := a.activeDoc(ctx, a.now())
	if err := a.Store.Clear(ctx); err != nil {
		return fmt.Errorf("workbox is asleep, but any keep-awake hold or scheduled sleep "+
			"could not be cleared (run `workbox cancel` to retry): %w", err)
	}
	if loadErr != nil {
		a.printClearedUnknown(loadErr)
	} else {
		a.printCancelledSleep(doc.Sleep)
		a.printClearedHold(doc.Hold)
	}
	a.printf("workbox is asleep.\n")
	return nil
}

// SleepAt schedules a one-off suspend at the next occurrence of HH:MM (or HHMM)
// in the configured timezone. It forces a suspend even within working hours.
func (a *App) SleepAt(ctx context.Context, hhmm string) error {
	dt, err := schedule.ParseDayTime(normalizeHHMM(hhmm))
	if err != nil {
		// Echo what the user typed, not the normalized "HH:MM" form.
		return fmt.Errorf("invalid time %q: want HH:MM or HHMM, 00:00-23:59", hhmm)
	}
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	span := a.Sched.ScheduledSleep(now, dt)
	// Only one scheduled sleep is kept, so one set for a different time is replaced.
	var replaced *schedule.Span
	if prev := doc.Sleep; prev != nil && !prev.Start.Equal(span.Start) {
		replaced = prev
	}
	// Any keep-awake hold is left in place: if the two overlap, the engine's
	// precedence (scheduled sleep over hold) decides.
	doc.Set(state.KindSleep, span)
	if err := a.Store.Save(ctx, doc); err != nil {
		return err
	}
	if replaced != nil {
		a.printf("Replaced the scheduled sleep at %s.\n", a.fmtTime(replaced.Start))
	}
	a.printf("Scheduled sleep: workbox suspends at %s and is held asleep until %s; "+
		"it never wakes on its own — run `workbox wake` to resume.\n",
		a.fmtTime(span.Start), a.fmtTime(span.End))
	if doc.Hold != nil && doc.Hold.End.After(span.Start) {
		a.printf("The keep-awake hold until %s stays in place, but the scheduled sleep wins from %s.\n",
			a.fmtTime(doc.Hold.End), a.fmtTime(span.Start))
	}
	a.printf("Run `workbox cancel` before then to call it off (this also clears any keep-awake hold).\n")
	return nil
}

// KeepAwake establishes a hold keeping the VM awake for at least d, waking it now
// if necessary. A scheduled sleep the *requested* hold overlaps is cancelled; a
// later one — or one covered only by a longer pre-existing hold — is left in
// place. As in Wake, the wake is retried for a bounded window when the zone has no
// capacity, and a wake that waited re-measures the hold from the post-wake clock
// so d is counted from when the VM is actually awake; a sleep that came into effect
// during that wait is cancelled as well, whichever hold is on record.
func (a *App) KeepAwake(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return errors.New("duration must be positive")
	}
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	req := a.Sched.KeepAwakeHold(now, d)
	hold, kept := longerAwakeHold(doc, req)
	doc.Set(state.KindHold, hold)
	// Only the hold actually requested cancels a sleep; a longer existing hold
	// that is merely preserved must not.
	var cancelled *schedule.Span
	if doc.Sleep != nil && req.End.After(doc.Sleep.Start) {
		cancelled, doc.Sleep = doc.Sleep, nil
	}
	if err := a.Store.Save(ctx, doc); err != nil {
		return err
	}
	a.printCancelledSleep(cancelled)
	a.printHold(hold, kept, a.idleDisabledNote(doc.Sleep))
	// A surviving scheduled sleep still wins over the hold where they overlap.
	a.printPendingSleep(doc.Sleep)
	// `keep-awake 3h` promises three hours of awake VM, so a capacity wait must
	// not be taken out of the hold reported above — a short one could otherwise
	// expire before the VM is even up. The re-measured hold cancels a scheduled
	// sleep it now reaches over, exactly as the requested one did.
	if err := a.wakeWithRetry(ctx, &holdTopUp{d: d}); err != nil {
		return err
	}
	a.printf("workbox is awake.\n")
	return nil
}

// Cancel removes any scheduled sleep and keep-awake hold, returning the VM to
// plain working-hours + idle-shutdown behavior.
func (a *App) Cancel(ctx context.Context) error {
	// Clear unconditionally, and regardless of whether the read succeeded:
	// activeDoc's pruned view can be empty while a document of already-expired
	// spans still exists, and the reconciler only collects documents while the
	// VM is running.
	doc, loadErr := a.activeDoc(ctx, a.now())
	if err := a.Store.Clear(ctx); err != nil {
		return err
	}
	if loadErr != nil {
		a.printClearedUnknown(loadErr)
		return nil
	}
	if doc.Empty() {
		a.printf("Nothing to clear: no scheduled sleep or keep-awake hold was in effect.\n")
		return nil
	}
	a.printCancelledSleep(doc.Sleep)
	a.printClearedHold(doc.Hold)
	return nil
}

// printPendingSleep reminds the user of a scheduled sleep a command left in
// place, since it will still suspend the VM (it wins over a keep-awake hold).
func (a *App) printPendingSleep(sp *schedule.Span) {
	if sp != nil {
		a.printf("Scheduled sleep still set for %s "+
			"(`workbox cancel` calls it off — and clears any keep-awake hold).\n",
			a.fmtTime(sp.Start))
	}
}

// printClearedUnknown reports a clear whose prior contents could not be read.
func (a *App) printClearedUnknown(err error) {
	a.printf("Cleared any keep-awake hold and scheduled sleep (could not read what was in effect: %v).\n", err)
}

// printClearedHold reports a keep-awake hold a command cleared as a side effect,
// so it doesn't silently disappear.
func (a *App) printClearedHold(sp *schedule.Span) {
	if sp != nil {
		a.printf("Cleared the keep-awake hold (was set until %s).\n", a.fmtTime(sp.End))
	}
}

// printCancelledSleep reports a scheduled sleep a command cancelled as a side
// effect, so it doesn't silently disappear.
func (a *App) printCancelledSleep(sp *schedule.Span) {
	if sp != nil {
		a.printf("Cancelled the scheduled sleep at %s.\n", a.fmtTime(sp.Start))
	}
}

// printHold reports the hold a command established (or preserved). It describes
// the stored hold rather than the machine, since the resume can still fail.
// note annotates the message in either case; a caller passes it only when it is
// true of the hold being reported.
func (a *App) printHold(hold schedule.Span, kept bool, note string) {
	a.printf("%s", holdMessage(a.fmtTime(hold.End), kept, note))
}

// holdMessage is the single wording for an established or preserved hold, so
// `wake` and `keep-awake` cannot drift apart. note annotates either form (e.g.
// " (grace window)" for a hold wake just set).
func holdMessage(end string, kept bool, note string) string {
	if kept {
		return fmt.Sprintf("A longer keep-awake hold is already in place until %s%s.\n", end, note)
	}
	return fmt.Sprintf("Keep-awake hold set until %s%s.\n", end, note)
}

// longerAwakeHold returns candidate unless an existing awake hold already runs
// longer, in which case the existing hold is preserved (never shortened) and
// kept is true.
func longerAwakeHold(doc *state.Document, candidate schedule.Span) (hold schedule.Span, kept bool) {
	if existing := doc.Hold; existing != nil && existing.State == schedule.Awake && existing.End.After(candidate.End) {
		return *existing, true
	}
	return candidate, false
}

// fmtTime renders t in the schedule timezone.
func (a *App) fmtTime(t time.Time) string {
	return t.In(a.Sched.Loc).Format(schedule.TimeLayout)
}

// normalizeHHMM accepts the colon-less "2000" shorthand on the command line and
// returns the "HH:MM" form schedule.ParseDayTime expects; any other input is
// returned unchanged so the parser reports a clear error. Config times stay
// strictly HH:MM because Terraform parses them too.
func normalizeHHMM(s string) string {
	if len(s) != 4 {
		return s
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return s
		}
	}
	return s[:2] + ":" + s[2:]
}
