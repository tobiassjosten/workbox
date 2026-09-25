package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/state"
)

// Waking needs Compute Engine to place the exact machine shape in the zone at
// that moment — a suspended VM reserves no capacity, and a cold start reserves
// none either — so a wake can fail simply because the zone is full. That is
// transient and usually clears in minutes, so wake retries for a bounded window
// instead of handing the user a retry loop. The window bounds when a new attempt
// may *start*: an attempt already under way is never cut short, so a wake can
// run past it.
//
// The real ceiling is the hold the wake is protected by (idle timeout plus
// ssh.wait_timeout_seconds for a grace, or the requested duration for
// keep-awake): the reconciler suspends a RUNNING VM whose hold has lapsed, so
// the retry refreshes that hold between attempts — but only while it would
// otherwise lapse before the wake can be over — rather than only at the end.
// README.md and docs/operations.md quote the window as "about 5 minutes";
// TestCapacityDocsMatchTheCode fails if the constant and the prose drift apart.
// `doctor`'s long help says "a few minutes", deliberately vague and so not
// pinned.
const (
	capacityRetryWindow   = 5 * time.Minute
	capacityRetryInterval = 30 * time.Second
)

// capacitySection is the docs/operations.md heading the give-up report points
// at. Named here so the pointer and the docs cannot drift apart unnoticed;
// TestCapacityDocsMatchTheCode pins both this and the window.
const capacitySection = "Zone has no capacity for the machine type"

// holdTopUp is the hold a wake path keeps alive across a capacity wait. d is
// re-measured from the current clock each time, so the hold never carries the
// time already spent waiting.
//
// grace tells the two callers apart. Wake's hold is the grace window, and what a
// top-up writes is a grace window whatever hold it replaced, so it is always
// labelled one. Keep-awake's is the duration the user asked for: it re-applies
// that command's rule that the requested hold cancels a scheduled sleep it
// reaches over — wake deliberately leaves a future one alone — and its qualifier
// is re-derived when reported, since a sleep can have landed while we waited.
type holdTopUp struct {
	d     time.Duration
	grace bool
}

func (t *holdTopUp) cancelsSleepItReachesOver() bool { return !t.grace }

// waitFor waits d, honoring cancellation. Tests inject a.wait to advance a fake
// clock instead of the wall clock.
func (a *App) waitFor(ctx context.Context, d time.Duration) error {
	if a.wait != nil {
		return a.wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// wakeWithRetry wakes the VM, retrying while the zone has no capacity for its
// machine type. Every other failure — including an unsupported instance state —
// is returned on the first attempt. On giving up it reports the remedies and
// returns the last capacity error.
//
// top, when non-nil, is the hold protecting the woken VM: it is refreshed between
// attempts — see refreshHoldSilently for when, and why not always — and reported
// once at the end when re-measuring it from the post-wake clock extends what is
// on record.
func (a *App) wakeWithRetry(ctx context.Context, top *holdTopUp) error {
	var waited bool
	start := a.now()
	deadline := start.Add(capacityRetryWindow)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// compute.Wake re-reads the state each time, so an attempt after a
		// failed resume that left the VM TERMINATED starts it instead.
		err := compute.Wake(ctx, a.Compute)
		var capErr *compute.CapacityError
		if !errors.As(err, &capErr) {
			if err == nil && waited {
				a.settleAfterWait(ctx, top)
			}
			return err
		}
		capErr = capErr.WithDefaults(a.Cfg.GCP.Zone, a.Cfg.GCP.MachineType)
		now := a.now()
		left := deadline.Sub(now)
		if left <= 0 {
			a.capacityRemedy(capErr, attempt, now.Sub(start))
			return capErr
		}
		a.capacityNotice(capErr, attempt, left)
		waited = true
		// Refresh the hold before waiting again: the next attempt can leave the
		// VM RUNNING minutes from now, and the reconciler suspends a RUNNING VM
		// whose hold has lapsed.
		a.refreshHoldSilently(ctx, top, deadline)
		if err := a.waitFor(ctx, min(capacityRetryInterval, left)); err != nil {
			return err
		}
	}
}

// capacityNotice reports a capacity failure we are going to retry. The first
// one explains the situation; later ones are one short line each, so a wake
// that sits there for minutes never looks hung — which matters most in the
// ssh/herdr flows, where the user is waiting on a process handoff. Like the
// give-up report it goes to ErrOut, so the whole capacity narrative shares one
// stream and the progress survives a redirected stdout.
func (a *App) capacityNotice(c *compute.CapacityError, attempt int, left time.Duration) {
	if attempt == 1 {
		// Floored: "up to about N minutes" is a promise, so it must not round up
		// past what the window has left.
		a.errPrintf("%s — retrying for up to about %s...\n", c.Error(), atMostMinutes(left))
		return
	}
	a.errPrintf("Still no capacity in %s (about %s left)...\n", c.Zone, atLeastMinutes(left))
}

// capacityRemedy explains a wake we gave up on. Like diagnoseUnreachable in
// cmd/workbox, it prints the advice and leaves the caller to return the error
// itself. elapsed is the time actually spent — it runs past the retry window
// when the failing operations were slow — and is reported in whole minutes that
// passed. Server-supplied text is passed as an argument, never as part of the
// format: a % in it would otherwise be read as a verb.
func (a *App) capacityRemedy(c *compute.CapacityError, attempts int, elapsed time.Duration) {
	a.errPrintf("\nNo capacity for %s in %s after %s across %s.\n",
		c.MachineType, c.Zone, attemptsText(attempts), atMostMinutes(elapsed))
	if len(c.ZonesAvailable) > 0 {
		a.errPrintf("GCP reports capacity in: %s\n", strings.Join(c.ZonesAvailable, ", "))
	}
	a.errPrintf("%s", "The failed wake changed nothing about your disks, and the suspended\n"+
		"memory state is intact unless the VM has since been terminated;\n"+
		"`workbox status` says whether it is still SUSPENDED or now TERMINATED.\n"+
		"What you can do:\n"+
		"  - try again in a while; capacity shortages are usually transient\n"+
		"  - change gcp.machine_type and run `make tf-apply`: another machine series\n"+
		"    draws on a different pool, but the VM must stop to change shape, which\n"+
		"    discards the suspended memory state (the disks are untouched)\n"+
		"    — the new shape must still support suspend (no GPUs, no Local SSD,\n"+
		"      at most 208 GB of memory)\n"+
		"  - move to another zone — not just a config change: the development disk is\n"+
		"    zonal and protected, so it means migrating disk and instance by hand\n")
	if c.Message != "" {
		a.errPrintf("GCP says: %s\n", c.Message)
	}
	a.errPrintf("See docs/operations.md, %q, for the full options.\n\n", capacitySection)
}

// atLeastMinutes rounds up, so a window with seconds left in it is never
// reported as no time at all. Used for the per-attempt lines, where overstating
// by up to a minute is harmless.
func atLeastMinutes(d time.Duration) string {
	return minutesText(int((d + time.Minute - time.Nanosecond) / time.Minute))
}

// atMostMinutes rounds down, counting only whole minutes that passed or are
// certain to be available: the give-up line must not claim a minute that did not
// go by, and the opening promise must not name more time than the window has
// left. The single exception is minutesText's own floor — under a minute reads as
// one — which the give-up line cannot reach and the promise can only overstate by
// seconds.
func atMostMinutes(d time.Duration) string {
	return minutesText(int(d / time.Minute))
}

// minutesText spells out a whole number of minutes, never fewer than one: the
// messages that use it are about spans the user waited or will wait.
func minutesText(m int) string {
	if m <= 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// attemptsText spells out an attempt count, since the give-up line can report a
// single attempt when one slow failure outlasts the whole window.
func attemptsText(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}

// settleAfterWait re-applies both post-wait rules in one read-modify-write and
// then reports what changed. A wake that spent minutes waiting for zone capacity
// would otherwise hand the user less time than it said — with a short idle
// timeout, little enough that the reconciler could suspend the VM before the
// activity emitter first reports — and would leave in place a scheduled sleep
// that came into effect meanwhile, which outranks any hold and would suspend the
// VM this command just reported awake.
//
// The document is re-read here rather than reusing the one loaded before the
// wait, because Save replaces the whole document: another shell may have run
// `workbox sleep` or `workbox cancel` meanwhile, and writing back a stale
// snapshot would silently undo it. A document whose hold is gone — a cancel
// during the wait — is left alone rather than having the hold resurrected.
//
// This is a top-up rather than the operation the user asked for, so a failure is
// reported and the command carries on: the hold written before the wake stands.
func (a *App) settleAfterWait(ctx context.Context, top *holdTopUp) {
	now := a.now()
	doc, err := a.Store.Load(ctx)
	if err != nil {
		a.errPrintf("Could not re-check the keep-awake hold and scheduled sleep (%v); "+
			"run `workbox schedule` to see where they stand.\n", err)
		return
	}
	// The pruned view decides what counts as in effect, exactly as the pre-wake
	// checks do: a span whose state does not fit its slot is one the reconciler
	// ignores, and must not be reported as cancelled or treated as a live hold.
	active := doc.Active(now)

	hold := a.applyFreshHold(doc, active, top, now)
	cancelled, reason := a.sleepToCancel(doc, active, top, now)
	surviving := active.Sleep
	if cancelled != nil {
		doc.Sleep, surviving = nil, nil
	}
	if hold == nil && cancelled == nil {
		return
	}
	if err := a.saveOrClear(ctx, doc); err != nil {
		a.reportSettleFailure(top, hold, cancelled, reason, err)
		return
	}
	if hold != nil {
		// Re-measured rather than "extended": with minute resolution a short wait
		// often renders the same time the pre-wake line already printed.
		a.printf("Re-measured the keep-awake hold from now; it runs until %s%s, since the wake waited for capacity.\n",
			a.fmtTime(hold.End), a.holdNote(top, surviving))
	}
	if cancelled != nil {
		a.printf("Cancelled the scheduled sleep at %s: %s.\n", a.fmtTime(cancelled.Start), reason)
	}
}

// applyFreshHold re-measures top's hold from now and stores it on doc when that
// outlasts the live hold on record. A hold the document no longer holds is not
// resurrected; one whose state does not fit its slot counts as absent, since the
// reconciler honors only an awake hold.
func (a *App) applyFreshHold(doc, active *state.Document, top *holdTopUp, now time.Time) *schedule.Span {
	if top == nil || doc.Hold == nil {
		return nil
	}
	fresh := a.Sched.KeepAwakeHold(now, top.d)
	if active.Hold != nil && !fresh.End.After(active.Hold.End) {
		return nil
	}
	doc.Set(state.KindHold, fresh)
	return &fresh
}

// sleepToCancel picks the scheduled sleep this wake must call off, with the
// reason to report: one that came into effect while we waited (wake's own rule,
// re-applied at the post-wake clock), or — for keep-awake — one the requested
// hold now reaches over. The second test uses the re-measured *requested* hold,
// not whatever this pass wrote, so it matches the rule KeepAwake applied before
// the wait, under which a longer pre-existing hold that is merely preserved
// cancels nothing.
func (a *App) sleepToCancel(doc, active *state.Document, top *holdTopUp, now time.Time) (*schedule.Span, string) {
	if active.Sleep == nil {
		return nil, ""
	}
	if active.Sleep.Active(now) {
		return doc.Sleep, "it came into effect while the wake waited for capacity"
	}
	if top != nil && top.cancelsSleepItReachesOver() {
		if requested := a.Sched.KeepAwakeHold(now, top.d); requested.End.After(active.Sleep.Start) {
			return doc.Sleep, "the re-measured keep-awake hold now runs past it"
		}
	}
	return nil, ""
}

// holdNote qualifies the hold being reported; top is non-nil, since only a top-up
// produces a hold to report. Wake's top-up always writes a grace window, whatever
// hold it replaced. Keep-awake's qualifier is re-derived from the state that
// survives the wait, so it cannot claim nothing would auto-suspend while a
// scheduled sleep that will is on record.
func (a *App) holdNote(top *holdTopUp, sleep *schedule.Span) string {
	if top.grace {
		return " (grace window)"
	}
	return a.idleDisabledNote(sleep)
}

// reportSettleFailure explains a top-up that could not be written. When both
// halves were at stake it reports the sleep, the more urgent of the two: the one
// recovery command it names repairs both, and an uncancelled sleep suspends the
// VM whatever the hold says. The wake itself succeeded.
func (a *App) reportSettleFailure(top *holdTopUp, hold, cancelled *schedule.Span, reason string, err error) {
	// The command that redoes exactly what failed: `wake` re-measures the grace
	// and cancels only an in-effect sleep, `keep-awake` restores the duration the
	// user asked for. Both resume the VM, and resuming a running one is a no-op,
	// so the hint holds whether or not the reconciler got there first — whereas
	// `workbox cancel` would clear the sleep and leave the machine suspended.
	// Keying this on the top-up kind matters: telling a wake user to run
	// keep-awake would cancel a future scheduled sleep the wake path preserves.
	recoverCmd := "`workbox wake`"
	if top != nil && top.cancelsSleepItReachesOver() {
		recoverCmd = fmt.Sprintf("`workbox keep-awake %s`", shortDuration(top.d))
	}
	if cancelled != nil {
		// A sleep already in effect is acted on by the next reconciler tick, so
		// the VM may well be suspended before the user can type anything: give
		// the recovery rather than a deadline to beat. A future sleep — the
		// keep-awake reach-over case — still leaves time to head it off.
		if cancelled.Active(a.now()) {
			a.errPrintf("The scheduled sleep at %s is in effect and could not be cancelled (%v); "+
				"it may suspend the VM within the minute — run %s to call it off and bring the VM back.\n",
				a.fmtTime(cancelled.Start), err, recoverCmd)
			return
		}
		a.errPrintf("The scheduled sleep at %s could not be cancelled (%v), though %s; "+
			"run %s to call it off before it suspends the VM.\n",
			a.fmtTime(cancelled.Start), err, reason, recoverCmd)
		return
	}
	if hold != nil {
		a.errPrintf("Could not extend the keep-awake hold (%v); it runs at least as long as "+
			"reported above — run %s to set it from now.\n",
			err, recoverCmd)
	}
}

// refreshHoldSilently keeps the hold from lapsing while we wait for capacity. It
// is silent because it runs once per attempt: on success settleAfterWait reports
// the end the user acts on, and on a give-up nothing is printed and the hold on
// record is whatever was written last — the pre-wake hold when no refresh was
// needed, otherwise the last one (see docs/operations.md). It never cancels a
// scheduled sleep — only the reported pass does, so the user is never left with
// one silently deleted — and a hold reaching past a sleep meanwhile is harmless,
// since scheduled sleep outranks a hold.
//
// A hold that will still be running when the wake can no longer be in flight
// needs no refreshing: skipping those writes keeps the common case to the single
// pre-wake write, and with it the window in which a concurrent `workbox sleep` or
// `cancel` could be lost. The cutoff is the deadline plus a whole window of
// slack, because the attempt running when the deadline passes is never cut short:
// that covers a hold which could lapse mid-wake — the short-idle configuration
// this top-up exists for — while assuming the final attempt does not itself
// outlast a whole window.
func (a *App) refreshHoldSilently(ctx context.Context, top *holdTopUp, deadline time.Time) {
	if top == nil {
		return
	}
	now := a.now()
	doc, err := a.Store.Load(ctx)
	if err != nil || doc.Hold == nil {
		return
	}
	active := doc.Active(now)
	if active.Hold != nil && active.Hold.End.After(deadline.Add(capacityRetryWindow)) {
		return
	}
	if hold := a.applyFreshHold(doc, active, top, now); hold != nil {
		_ = a.saveOrClear(ctx, doc)
	}
}

// shortDuration renders a hold length for a recovery hint, as the user would
// type it. Whole hours and minutes get the short forms; anything else falls back
// to Duration's own rendering, which time.ParseDuration accepts — the hint has to
// be a command that runs, and match the hold it replaces.
func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return d.String()
	}
}
