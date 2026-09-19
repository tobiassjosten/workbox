package cli

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/state"
)

// SpanJSON is the stable JSON form of a schedule span.
type SpanJSON struct {
	Start string `json:"start"`
	End   string `json:"end"`
	State string `json:"state"`
}

func spanJSON(s *schedule.Span) *SpanJSON {
	if s == nil {
		return nil
	}
	return &SpanJSON{
		Start: s.Start.Format(time.RFC3339),
		End:   s.End.Format(time.RFC3339),
		State: string(s.State),
	}
}

// TransitionJSON is the stable JSON form of the next transition.
type TransitionJSON struct {
	At string `json:"at"`
	To string `json:"to"`
}

// StatusJSON is the stable machine-readable status document.
type StatusJSON struct {
	Name     string `json:"name"`
	VM       string `json:"vm"`
	Desired  string `json:"desired"`
	Schedule struct {
		Wake     string `json:"wake"`
		Sleep    string `json:"sleep"`
		Timezone string `json:"timezone"`
	} `json:"schedule"`
	NextTransition *TransitionJSON `json:"next_transition,omitempty"`
	NextWake       string          `json:"next_wake,omitempty"`
	NextSleep      string          `json:"next_sleep,omitempty"`
	WakeOverride   *SpanJSON       `json:"wake_override,omitempty"`
	SleepOverride  *SpanJSON       `json:"sleep_override,omitempty"`
	Hold           *SpanJSON       `json:"hold,omitempty"`
	SSH            struct {
		Target    string `json:"target"`
		Available bool   `json:"available"`
		Reason    string `json:"reason,omitempty"`
	} `json:"ssh"`
}

// collectStatus gathers the current status without any external SSH probing. The
// returned time.Time is the next-transition instant (zero if none); PrintStatus
// uses it to format the transition in the schedule timezone without re-parsing
// the RFC3339 string stored in StatusJSON.
func (a *App) collectStatus(ctx context.Context) (s StatusJSON, doc *state.Document, nextAt time.Time, err error) {
	now := a.now()

	vm, err := a.Compute.Status(ctx)
	if err != nil {
		return s, nil, time.Time{}, err
	}

	doc, err = a.activeDoc(ctx, now)
	if err != nil {
		return s, nil, time.Time{}, err
	}
	ov := doc.Overrides()

	s.Name = a.Cfg.Name
	s.VM = vm.String()
	s.Desired = string(a.Sched.Desired(now, ov))
	s.Schedule.Wake = a.Cfg.Schedule.Wake
	s.Schedule.Sleep = a.Cfg.Schedule.Sleep
	s.Schedule.Timezone = a.Cfg.Schedule.Timezone

	if tr, ok := a.Sched.NextTransition(now, ov); ok {
		nextAt = tr.At
		s.NextTransition = &TransitionJSON{At: tr.At.Format(time.RFC3339), To: string(tr.To)}
	}
	if nw, ok := a.Sched.NextWake(now, ov); ok {
		s.NextWake = nw.Format(time.RFC3339)
	}
	if ns, ok := a.Sched.NextSleep(now, ov); ok {
		s.NextSleep = ns.Format(time.RFC3339)
	}
	s.WakeOverride = spanJSON(doc.Wake)
	s.SleepOverride = spanJSON(doc.Sleep)
	s.Hold = spanJSON(doc.Hold)

	s.SSH.Target = a.Cfg.Tailscale.SSHTarget
	// SSH is only usable once the VM is fully RUNNING; transitional states don't count.
	s.SSH.Available = vm == compute.Running
	if !s.SSH.Available {
		s.SSH.Reason = "VM is " + s.VM
	}
	return s, doc, nextAt, nil
}

// PrintStatusJSON prints the machine-readable status.
func (a *App) PrintStatusJSON(ctx context.Context) error {
	s, _, _, err := a.collectStatus(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// PrintStatus prints the human-readable status.
func (a *App) PrintStatus(ctx context.Context) error {
	s, doc, nextAt, err := a.collectStatus(ctx)
	if err != nil {
		return err
	}
	a.printf("VM:              %s\n", s.VM)
	a.printf("Desired:         %s\n", s.Desired)
	a.printf("Schedule:        %s–%s %s\n", s.Schedule.Wake, s.Schedule.Sleep, s.Schedule.Timezone)
	if s.NextTransition != nil {
		verb := "resume"
		if s.NextTransition.To == string(schedule.Asleep) {
			verb = "suspend"
		}
		a.printf("Next transition: %s at %s\n", verb, a.fmtTime(nextAt))
	}
	a.printf("Override:        %s\n", a.describeOverrides(doc))
	a.printf("Hold:            %s\n", a.describeHold(doc))
	if s.SSH.Available {
		a.printf("Tailscale/SSH:   target %s (VM running)\n", s.SSH.Target)
	} else {
		a.printf("Tailscale/SSH:   unavailable because %s\n", s.SSH.Reason)
	}
	return nil
}

func (a *App) describeOverrides(doc *state.Document) string {
	switch {
	case doc.Wake != nil && doc.Sleep != nil:
		return "wake " + a.fmtTime(wakeEdge(doc.Wake)) + "; sleep " + a.fmtTime(sleepEdge(doc.Sleep))
	case doc.Wake != nil:
		return "wake " + a.fmtTime(wakeEdge(doc.Wake))
	case doc.Sleep != nil:
		return "sleep " + a.fmtTime(sleepEdge(doc.Sleep))
	default:
		return "none"
	}
}

// wakeEdge returns the user-meaningful transition edge of a wake override span.
// For early-wake (Awake) spans the requested wake time is Start; for delayed-wake
// (Asleep) spans it is End.
func wakeEdge(s *schedule.Span) time.Time {
	if s == nil {
		return time.Time{}
	}
	if s.State == schedule.Awake {
		return s.Start
	}
	return s.End
}

// sleepEdge returns the user-meaningful transition edge of a sleep override span.
// For extend-awake (Awake) spans the requested sleep time is End; for early-sleep
// (Asleep) spans it is Start.
func sleepEdge(s *schedule.Span) time.Time {
	if s == nil {
		return time.Time{}
	}
	if s.State == schedule.Awake {
		return s.End
	}
	return s.Start
}

func (a *App) describeHold(doc *state.Document) string {
	if doc.Hold == nil {
		return "none"
	}
	return "stay " + string(doc.Hold.State) + " until " + a.fmtTime(doc.Hold.End)
}

// Schedule prints the normal schedule, overrides, holds and next transitions.
// It only reads the state document and computes transitions — it does not call
// the Compute API, so it works even when the VM is unreachable.
func (a *App) Schedule(ctx context.Context) error {
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	ov := doc.Overrides()
	a.printf("Normal schedule: wake %s, sleep %s (%s)\n",
		a.Cfg.Schedule.Wake, a.Cfg.Schedule.Sleep, a.Cfg.Schedule.Timezone)
	a.printf("Override:        %s\n", a.describeOverrides(doc))
	a.printf("Hold:            %s\n", a.describeHold(doc))
	if nw, ok := a.Sched.NextWake(now, ov); ok {
		a.printf("Next wake:       %s\n", a.fmtTime(nw))
	}
	if ns, ok := a.Sched.NextSleep(now, ov); ok {
		a.printf("Next sleep:      %s\n", a.fmtTime(ns))
	}
	a.printf("\nTo permanently change the schedule, edit schedule.wake/schedule.sleep in\nyour config and re-run `make tf-apply`.\n")
	return nil
}
