package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// WorkingHoursJSON reports the working-hours window. It is always present;
// Enabled is false (and the rest zero) when no window is configured or the
// window is disabled (`enabled: false`), and an absent days list means the
// window applies every day.
type WorkingHoursJSON struct {
	Enabled bool     `json:"enabled"`
	Start   string   `json:"start,omitempty"`
	End     string   `json:"end,omitempty"`
	Days    []string `json:"days,omitempty"`
	Within  bool     `json:"within"`
}

// ActivityJSON reports the last-active readout when known. IdleSeconds measures
// from LastActive alone; auto-suspend counts idle from the later of LastActive
// and StatusJSON.LastStart, so during boot grace the two differ.
type ActivityJSON struct {
	LastActive  string `json:"last_active"`
	IdleSeconds int64  `json:"idle_seconds"`
}

// StatusJSON is the stable machine-readable status document.
type StatusJSON struct {
	Name     string `json:"name"`
	VM       string `json:"vm"`
	Schedule struct {
		Timezone           string           `json:"timezone"`
		WorkingHours       WorkingHoursJSON `json:"working_hours"`
		IdleTimeoutMinutes int              `json:"idle_timeout_minutes"`
	} `json:"schedule"`
	AutoSuspend struct {
		// Suspend is meaningful only when Reason is one of the schedule.Reason*
		// values: for ReasonNotRunning and ReasonActivityUnknown the verdict is
		// unknown, not "no", so consumers must branch on Reason first.
		Suspend bool   `json:"suspend"`
		Reason  string `json:"reason"`
	} `json:"auto_suspend"`
	Activity *ActivityJSON `json:"activity,omitempty"`
	// ActivityIgnored explains a last-active value `workbox status` read but
	// disregarded: not a usable timestamp, or too far in the future.
	ActivityIgnored string `json:"activity_ignored,omitempty"`
	// ActivityError and LastStartError report a failed read, so a consumer can
	// tell "could not read" from "nothing reported".
	ActivityError string `json:"activity_error,omitempty"`
	// LastStart is the instance's last start while it is running; idle shutdown
	// counts from the later of it and activity.last_active.
	LastStart      string    `json:"last_start,omitempty"`
	LastStartError string    `json:"last_start_error,omitempty"`
	KeepAwake      *SpanJSON `json:"keep_awake,omitempty"`
	ScheduledSleep *SpanJSON `json:"scheduled_sleep,omitempty"`
	SSH            struct {
		Target    string `json:"target"`
		Available bool   `json:"available"`
		Reason    string `json:"reason,omitempty"`
	} `json:"ssh"`
}

// Status-only auto-suspend reasons, for when the CLI cannot reproduce the
// reconciler's verdict. The reconciler itself only acts on a RUNNING VM. With
// the schedule.Reason* constants these are the complete set of values
// StatusJSON.AutoSuspend.Reason can take.
const (
	ReasonNotRunning      = "not-running"
	ReasonActivityUnknown = "activity-unknown"
)

// statusData is everything collectStatus gathers for the two renderers.
type statusData struct {
	Now         time.Time
	VM          compute.State
	JSON        StatusJSON
	Doc         *state.Document
	LastActive  time.Time
	HaveActive  bool
	LastStart   time.Time
	ActivityErr error
	// Ignored explains a read value the verdict disregards (the reconciler never
	// counts it as recent activity either); empty when none was ignored.
	Ignored  string
	StartErr error
}

// collectStatus gathers the current status without any external SSH probing.
func (a *App) collectStatus(ctx context.Context) (statusData, error) {
	now := a.now()
	d := statusData{Now: now}

	vm, err := a.Compute.Status(ctx)
	if err != nil {
		return d, err
	}
	d.VM = vm

	d.Doc, err = a.activeDoc(ctx, now)
	if err != nil {
		return d, err
	}

	// Activity is only meaningful while the VM is running. A read failure is
	// reported rather than mistaken for "no activity".
	if a.Activity != nil && vm == compute.Running {
		// The two reads are independent: an unreadable activity value still
		// leaves the boot grace to decide here, whereas the reconciler aborts
		// the tick on a non-404 read error and retries the next minute.
		d.LastActive, d.HaveActive, d.ActivityErr = a.Activity.LastActive(ctx)
		switch {
		case errors.Is(d.ActivityErr, compute.ErrInvalidActivity):
			// Drop the value so the verdict counts no activity; the reconciler
			// never counts such a value as recent activity either.
			d.Ignored, d.ActivityErr = "not a usable unix timestamp", nil
		case d.ActivityErr == nil && d.HaveActive && schedule.FutureActivity(d.LastActive, now):
			d.Ignored = "last active " + a.fmtTime(d.LastActive) + " is in the future"
		}
		if d.ActivityErr != nil || d.Ignored != "" {
			d.LastActive, d.HaveActive = time.Time{}, false
		}
		d.LastStart, _, d.StartErr = a.Activity.LastStart(ctx)
	}

	idle := a.Cfg.Schedule.IdleTimeout()
	s := &d.JSON
	s.Name = a.Cfg.Name
	s.VM = vm.String()
	s.Schedule.Timezone = a.Cfg.Schedule.Timezone
	s.Schedule.IdleTimeoutMinutes = int(idle / time.Minute)
	if a.Sched.Enabled {
		s.Schedule.WorkingHours = WorkingHoursJSON{
			Enabled: true,
			Start:   a.Sched.Start.String(),
			End:     a.Sched.End.String(),
			Days:    shortDays(a.Sched.Days),
			Within:  a.Sched.WithinWorkingHours(now),
		}
	}
	if vm != compute.Running {
		s.AutoSuspend.Reason = ReasonNotRunning
	} else {
		act := schedule.Activity{LastActive: d.LastActive, LastStart: d.LastStart}
		dec := a.Sched.AutoSuspend(now, d.Doc.Spans(), act, idle)
		activityUnread := a.Activity == nil || d.ActivityErr != nil
		suspendsOnIdle := dec.Reason == schedule.ReasonNoActivity || dec.Reason == schedule.ReasonIdle
		if (activityUnread && dec.Reason == schedule.ReasonNoActivity) || (d.StartErr != nil && suspendsOnIdle) {
			// The verdict hinged on a value we could not (or do not) read: the
			// activity, or the start time that could have meant boot grace.
			s.AutoSuspend.Reason = ReasonActivityUnknown
		} else {
			s.AutoSuspend.Suspend = dec.Suspend
			s.AutoSuspend.Reason = dec.Reason
		}
	}
	if d.HaveActive {
		s.Activity = &ActivityJSON{
			LastActive:  d.LastActive.Format(time.RFC3339),
			IdleSeconds: int64(idleFor(now, d.LastActive) / time.Second),
		}
	}
	s.ActivityIgnored = d.Ignored
	if d.ActivityErr != nil {
		s.ActivityError = d.ActivityErr.Error()
	}
	if d.StartErr != nil {
		s.LastStartError = d.StartErr.Error()
	}
	if !d.LastStart.IsZero() {
		s.LastStart = d.LastStart.Format(time.RFC3339)
	}
	s.KeepAwake = spanJSON(d.Doc.Hold)
	s.ScheduledSleep = spanJSON(d.Doc.Sleep)

	s.SSH.Target = a.Cfg.Tailscale.SSHTarget
	s.SSH.Available = vm == compute.Running
	if !s.SSH.Available {
		s.SSH.Reason = "VM is " + s.VM
	}
	return d, nil
}

// PrintStatusJSON prints the machine-readable status.
func (a *App) PrintStatusJSON(ctx context.Context) error {
	d, err := a.collectStatus(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(d.JSON)
}

// PrintStatus prints the human-readable status.
func (a *App) PrintStatus(ctx context.Context) error {
	d, err := a.collectStatus(ctx)
	if err != nil {
		return err
	}
	s, doc, now := d.JSON, d.Doc, d.Now

	a.printf("VM:              %s\n", s.VM)
	a.printWorkingHours(now)
	a.printIdle()
	switch {
	case d.ActivityErr != nil:
		a.printf("Activity:        unavailable (%v); run `workbox doctor`\n", d.ActivityErr)
	case d.Ignored != "":
		a.printf("Activity:        ignored (%s); run `workbox doctor`\n", d.Ignored)
	case d.HaveActive:
		a.printf("Activity:        last active %s (idle %s)\n",
			a.fmtTime(d.LastActive), humanDuration(idleFor(now, d.LastActive)))
	case a.Activity != nil && d.VM == compute.Running:
		// Running with no value: the startup script reports one at every boot, so
		// provisioning has not run since the upgrade that added that report, or
		// the report failed (see doctor).
		a.printf("Activity:        none reported; run `workbox doctor` if this persists\n")
	}
	if d.StartErr != nil {
		a.printf("Last start:      unavailable (%v); retry, and run `workbox doctor` if it persists\n", d.StartErr)
	}
	verdict := describeDecision(s.AutoSuspend.Suspend, s.AutoSuspend.Reason, s.VM)
	if r := s.AutoSuspend.Reason; r == schedule.ReasonActive || r == schedule.ReasonBootGrace {
		// Idle counts from the later of the two, as in schedule.AutoSuspend.
		ref := d.LastActive
		if d.LastStart.After(ref) {
			ref = d.LastStart
		}
		// Working hours would inhibit a suspend due inside the window, so only
		// name a time the reconciler could actually act on.
		if earliest := ref.Add(a.Cfg.Schedule.IdleTimeout()); !a.Sched.WithinWorkingHours(earliest) {
			verdict += fmt.Sprintf(" (earliest idle suspend %s)", a.fmtTime(earliest))
		}
	}
	a.printf("Auto-suspend:    %s\n", verdict)
	a.printHoldAndSleep(doc, now, withoutNone)
	if s.SSH.Available {
		a.printf("Tailscale/SSH:   target %s (VM running)\n", s.SSH.Target)
	} else {
		a.printf("Tailscale/SSH:   unavailable because %s\n", s.SSH.Reason)
	}
	return nil
}

// withNone/withoutNone select whether printHoldAndSleep prints an explicit
// "none" line for an absent hold or scheduled sleep.
const (
	withNone    = true
	withoutNone = false
)

// printHoldAndSleep renders the keep-awake hold and scheduled sleep. showNone
// prints an explicit "none" for each (the `schedule` view); status omits them.
func (a *App) printHoldAndSleep(doc *state.Document, now time.Time, showNone bool) {
	switch {
	case doc.Hold != nil:
		a.printf("Keep-awake:      until %s\n", a.fmtTime(doc.Hold.End))
	case showNone:
		a.printf("Keep-awake:      none\n")
	}
	switch {
	case doc.Sleep != nil:
		a.printf("Scheduled sleep: %s\n", a.describeSleep(doc.Sleep, now))
	case showNone:
		a.printf("Scheduled sleep: none\n")
	}
}

// describeSleep renders a scheduled sleep: its start while pending, and its
// span once in effect (the start alone would read as a time in the past).
func (a *App) describeSleep(sp *schedule.Span, now time.Time) string {
	if sp.Active(now) {
		return fmt.Sprintf("in effect since %s until %s", a.fmtTime(sp.Start), a.fmtTime(sp.End))
	}
	return "at " + a.fmtTime(sp.Start)
}

func (a *App) printWorkingHours(now time.Time) {
	idleOn := a.Cfg.Schedule.IdleTimeout() > 0
	if !a.Sched.Enabled {
		label := "none"
		if a.Cfg.Schedule.WorkingHours != nil {
			label = "disabled in config"
		}
		// Name the timezone here too: `sleep HH:MM` resolves against it.
		if idleOn {
			a.printf("Working hours:   %s (times in %s; idle shutdown always applies)\n", label, a.Cfg.Schedule.Timezone)
		} else {
			a.printf("Working hours:   %s (times in %s)\n", label, a.Cfg.Schedule.Timezone)
		}
		return
	}
	within := a.Sched.WithinWorkingHours(now)
	phase := "outside — idle shutdown active"
	switch {
	case !idleOn && within:
		phase = "within — idle shutdown disabled"
	case !idleOn:
		phase = "outside — idle shutdown disabled"
	case within:
		phase = "within — idle shutdown paused"
	}
	a.printf("Working hours:   %s–%s %s %s (%s)\n",
		a.Sched.Start, a.Sched.End, daysLabel(a.Sched.Days), a.Cfg.Schedule.Timezone, phase)
}

// daysLabel renders the active weekdays for humans.
func daysLabel(days []time.Weekday) string {
	set := weekdaySet(days)
	if len(set) == 0 || len(set) == 7 {
		return "every day"
	}
	weekdaysOnly := len(set) == 5 && set[time.Monday] && set[time.Tuesday] &&
		set[time.Wednesday] && set[time.Thursday] && set[time.Friday]
	if weekdaysOnly {
		return "Mon–Fri"
	}
	if len(set) == 2 && set[time.Saturday] && set[time.Sunday] {
		return "Sat–Sun"
	}
	return strings.Join(shortDaysFromSet(set), ", ")
}

// shortDays renders weekdays as 3-letter names in calendar order (Sun–Sat).
func shortDays(days []time.Weekday) []string {
	return shortDaysFromSet(weekdaySet(days))
}

// shortDaysFromSet is shortDays for callers that already built the set.
func shortDaysFromSet(set map[time.Weekday]bool) []string {
	var out []string
	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		if set[wd] {
			out = append(out, wd.String()[:3])
		}
	}
	return out
}

// weekdaySet returns the distinct weekdays in days.
func weekdaySet(days []time.Weekday) map[time.Weekday]bool {
	set := make(map[time.Weekday]bool, len(days))
	for _, d := range days {
		set[d] = true
	}
	return set
}

func (a *App) printIdle() {
	idle := a.Cfg.Schedule.IdleTimeout()
	switch {
	case idle <= 0:
		a.printf("Idle shutdown:   disabled\n")
	case a.Sched.Enabled:
		a.printf("Idle shutdown:   after %s of inactivity (outside working hours)\n", humanDuration(idle))
	default:
		a.printf("Idle shutdown:   after %s of inactivity\n", humanDuration(idle))
	}
}

// describeDecision renders the auto-suspend verdict for humans.
func describeDecision(suspend bool, reason, vm string) string {
	switch reason {
	case ReasonNotRunning:
		return "n/a — VM is " + vm
	case ReasonActivityUnknown:
		return "unknown — activity or start time could not be read"
	case schedule.ReasonScheduledSleep:
		return "yes — a scheduled sleep is due"
	case schedule.ReasonKeepAwake:
		return "no — held awake"
	case schedule.ReasonWorkingHours:
		return "no — within working hours"
	case schedule.ReasonIdleDisabled:
		return "no — idle shutdown disabled"
	case schedule.ReasonActive:
		return "no — recently active"
	case schedule.ReasonBootGrace:
		return "no — recently started"
	case schedule.ReasonIdle:
		return "yes — idle past the timeout"
	case schedule.ReasonNoActivity:
		return "yes — no activity reported"
	default:
		if suspend {
			return "yes"
		}
		return "no"
	}
}

// idleFor is how long the VM has been idle, clamped at zero so clock skew
// between the VM and this machine never reads as a negative idle time.
func idleFor(now, lastActive time.Time) time.Duration {
	return max(now.Sub(lastActive), 0)
}

// humanDuration renders a duration as a compact "1h2m" / "2h" / "45m" string.
func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 && m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// Schedule prints the working-hours window, idle-shutdown setting, active
// keep-awake hold and scheduled sleep. It reads only the state document and
// config, so it works even when the VM is unreachable.
func (a *App) Schedule(ctx context.Context) error {
	now := a.now()
	doc, err := a.activeDoc(ctx, now)
	if err != nil {
		return err
	}
	a.printWorkingHours(now)
	a.printIdle()
	a.printHoldAndSleep(doc, now, withNone)
	a.printf("\nWake is manual (`workbox` / `workbox wake`). To change working hours or\n" +
		"the idle timeout, edit the schedule block in your config and re-run\n" +
		"`make tf-apply`.\n")
	return nil
}
