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
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
	Out io.Writer
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

// activeDoc loads the state document with expired spans pruned as of now.
func (a *App) activeDoc(ctx context.Context, now time.Time) (*state.Document, error) {
	doc, err := a.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	return doc.Active(now), nil
}

// Wake resumes/starts the VM now and, if the baseline wants it asleep,
// establishes a stay-awake hold so the reconciler does not suspend it again.
func (a *App) Wake(ctx context.Context) error {
	return a.powerNow(ctx, a.Sched.WakeHold, compute.Wake,
		"Holding awake until %s.\n", "workbox is awake.\n")
}

// Sleep suspends the VM now and, if the baseline wants it awake, establishes a
// stay-asleep hold so the reconciler does not resume it.
func (a *App) Sleep(ctx context.Context) error {
	return a.powerNow(ctx, a.Sched.SleepHold, compute.Sleep,
		"Holding asleep until %s.\n", "workbox is asleep.\n")
}

// powerNow loads the active document, conditionally saves a hold before the
// compute call (so the reconciler retries on failure), runs the compute action,
// then prints the hold and done messages.
func (a *App) powerNow(
	ctx context.Context,
	holdFn func(time.Time, schedule.Overrides) (schedule.Span, bool),
	computeFn func(context.Context, compute.Compute) error,
	holdFmt, doneMsg string,
) error {
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	var holdMsg string
	// The hold is saved before the compute call so that if the call fails, the
	// next reconciler tick (≤1 min) sees the hold and retries the transition.
	if hold, need := holdFn(now, doc.Overrides()); need {
		doc.Set(state.KindHold, hold)
		if err := a.Store.Save(ctx, doc); err != nil {
			return err
		}
		holdMsg = fmt.Sprintf(holdFmt, a.fmtTime(hold.End))
	}
	if err := computeFn(ctx, a.Compute); err != nil {
		return err
	}
	// Print after the compute call so no output is emitted if the operation fails.
	if holdMsg != "" {
		a.printf("%s", holdMsg)
	}
	a.printf("%s", doneMsg)
	return nil
}

// WakeAt sets a one-workday wake override for the next relevant wake transition.
func (a *App) WakeAt(ctx context.Context, hhmm string) error {
	dt, err := schedule.ParseDayTime(hhmm)
	if err != nil {
		return err
	}
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	span, ok := a.Sched.WakeOverride(now, dt)
	if !ok {
		a.printf("%s is already the normal wake time; nothing to do.\n", hhmm)
		return nil
	}
	doc.Set(state.KindWake, span)
	if err := a.Store.Save(ctx, doc); err != nil {
		return err
	}
	a.describeWakeOverride(span)
	return nil
}

// SleepAt sets a one-workday sleep override for the current/next relevant sleep.
func (a *App) SleepAt(ctx context.Context, hhmm string) error {
	dt, err := schedule.ParseDayTime(hhmm)
	if err != nil {
		return err
	}
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	span, ok := a.Sched.SleepOverride(now, dt)
	if !ok {
		a.printf("%s is already the normal sleep time; nothing to do.\n", hhmm)
		return nil
	}
	doc.Set(state.KindSleep, span)
	if err := a.Store.Save(ctx, doc); err != nil {
		return err
	}
	a.describeSleepOverride(span)
	return nil
}

// KeepAwake establishes a hold keeping the VM awake for at least d, and wakes it
// now if necessary.
func (a *App) KeepAwake(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return errors.New("duration must be positive")
	}
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	hold := a.Sched.KeepAwakeHold(now, d)
	// Don't shorten an existing longer awake hold; always overwrite an asleep hold.
	if existing := doc.Hold; existing != nil && existing.State == schedule.Awake && existing.End.After(hold.End) {
		hold = *existing
	}
	doc.Set(state.KindHold, hold)
	if err := a.Store.Save(ctx, doc); err != nil {
		return err
	}
	if err := compute.Wake(ctx, a.Compute); err != nil {
		return err
	}
	a.printf("Keeping awake until %s.\n", a.fmtTime(hold.End))
	return nil
}

// CancelOverride removes all overrides and holds.
func (a *App) CancelOverride(ctx context.Context) error {
	if err := a.Store.Clear(ctx); err != nil {
		return err
	}
	a.printf("Cleared all overrides and holds; back to the normal schedule.\n")
	return nil
}

func (a *App) describeWakeOverride(span schedule.Span) {
	if span.State == schedule.Awake {
		a.printf("Early wake: workbox will wake at %s (instead of %s).\n",
			a.fmtTime(span.Start), a.fmtTime(span.End))
	} else {
		a.printf("Delayed wake: workbox will stay asleep until %s (instead of %s).\n",
			a.fmtTime(span.End), a.fmtTime(span.Start))
	}
	a.printAppliesOnce(a.Cfg.Schedule.Wake, "wake")
}

func (a *App) describeSleepOverride(span schedule.Span) {
	if span.State == schedule.Awake {
		a.printf("Delayed sleep: workbox will stay awake until %s (instead of %s).\n",
			a.fmtTime(span.End), a.fmtTime(span.Start))
	} else {
		a.printf("Early sleep: workbox will suspend at %s (instead of %s).\n",
			a.fmtTime(span.Start), a.fmtTime(span.End))
	}
	a.printAppliesOnce(a.Cfg.Schedule.Sleep, "sleep")
}

// printAppliesOnce reminds the user that a one-workday override does not affect
// future days. recurringTime is shown as bare HH:MM — the daily recurring time,
// not the time of this one occurrence.
func (a *App) printAppliesOnce(recurringTime, what string) {
	a.printf("This applies once; future days return to the normal %s %s.\n", recurringTime, what)
}

// fmtTime renders t in the schedule timezone.
func (a *App) fmtTime(t time.Time) string {
	return t.In(a.Sched.Loc).Format("Mon 2006-01-02 15:04 MST")
}
