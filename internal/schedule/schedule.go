// Package schedule decides when the workbox VM may be automatically suspended.
//
// The model has two independent, individually-optional controls:
//
//  1. Working hours: a recurring window [start, end) in a configured IANA
//     timezone, optionally limited to certain weekdays, during which automatic
//     suspend is disabled ("protected"). Wake is always manual — working hours
//     never resume the VM, they only inhibit sleep.
//  2. Idle shutdown: outside working hours (or when no window is configured), the
//     VM is suspended after a period of inactivity (the idle timeout), counted
//     from the later of its last reported activity and its last boot.
//
// Two operational spans, stored in Firestore, sit on top:
//   - a keep-awake hold (also used as a short grace window after a manual wake),
//     which inhibits suspend regardless of activity;
//   - a one-off scheduled sleep, which forces a suspend at a wall-clock time even
//     within working hours.
//
// Everything is expressed as absolute time.Time values, so DST transitions and
// midnight crossings come from the standard library's timezone database rather
// than manual offset arithmetic. The same decision runs in the Go CLI (status)
// and, step for step, in the GCP Workflow reconciler (infra/reconcile.yaml.tftpl;
// keep them in sync — the TestReconciler* tests in schedule_test.go check what
// they can: reason strings, the skew bound and the idle comparisons, plus the
// step order and precedence in assertDecisionOrder).
package schedule

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SpanState is what a Span asserts: an Awake span keeps the VM awake (a
// keep-awake hold), an Asleep span forces it asleep (a one-off scheduled
// sleep). The values are the Firestore wire encoding.
type SpanState string

const (
	// Awake marks a span that inhibits automatic suspend (keep-awake/grace).
	Awake SpanState = "awake"
	// Asleep marks a span that forces a suspend (one-off scheduled sleep).
	Asleep SpanState = "asleep"
)

// TimeLayout is how workbox renders an absolute time for humans (status,
// schedule and doctor all use it, so they name the same wall-clock time).
const TimeLayout = "Mon 2006-01-02 15:04 MST"

// DayTime is a wall-clock time of day (hour and minute) with no date.
type DayTime struct {
	Hour   int
	Minute int
}

// ParseDayTime parses an "HH:MM" 24-hour string.
func ParseDayTime(s string) (DayTime, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return DayTime{}, fmt.Errorf("invalid time %q: want HH:MM", s)
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 2 {
			return DayTime{}, fmt.Errorf("invalid time %q: want HH:MM", s)
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return DayTime{}, fmt.Errorf("invalid time %q: want HH:MM", s)
			}
		}
	}
	h, errH := strconv.Atoi(parts[0])
	m, errM := strconv.Atoi(parts[1])
	if errH != nil || errM != nil {
		return DayTime{}, fmt.Errorf("invalid time %q: want HH:MM", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return DayTime{}, fmt.Errorf("invalid time %q: hour must be 0-23 and minute 0-59", s)
	}
	return DayTime{Hour: h, Minute: m}, nil
}

// String renders the DayTime as "HH:MM".
func (d DayTime) String() string { return fmt.Sprintf("%02d:%02d", d.Hour, d.Minute) }

func minutes(d DayTime) int { return d.Hour*60 + d.Minute }

// Span is a half-open interval [Start, End) during which State applies.
type Span struct {
	Start time.Time
	End   time.Time
	State SpanState
}

// Active reports whether t falls within the span.
func (s *Span) Active(t time.Time) bool {
	if s == nil {
		return false
	}
	return !t.Before(s.Start) && t.Before(s.End)
}

// Expired reports whether the span has fully elapsed at time t and can be
// garbage-collected.
func (s *Span) Expired(t time.Time) bool {
	if s == nil {
		return true
	}
	return !t.Before(s.End)
}

// Spans carries the optional scheduled sleep and keep-awake hold read from
// Firestore.
type Spans struct {
	// Sleep is the one-off scheduled sleep. Asleep state.
	Sleep *Span
	// Hold is the keep-awake hold (also the post-wake grace window). Awake state.
	Hold *Span
}

// Schedule holds the working-hours window. When Enabled is false there is no
// protected window, so outside a keep-awake hold or scheduled sleep, idle
// shutdown alone decides auto-suspend. Days restricts the window to those
// weekdays; an empty Days means every day.
type Schedule struct {
	Enabled bool
	Start   DayTime
	End     DayTime
	Days    []time.Weekday
	Loc     *time.Location
}

// New builds an enabled working-hours schedule from HH:MM strings and an IANA
// timezone name. Start must be earlier in the day than End (same-day window).
// days restricts the window to those weekdays; pass none for every day.
func New(start, end, timezone string, days ...time.Weekday) (Schedule, error) {
	s, err := ParseDayTime(start)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule.working_hours.start: %w", err)
	}
	e, err := ParseDayTime(end)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule.working_hours.end: %w", err)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule.timezone %q: %w", timezone, err)
	}
	if minutes(s) >= minutes(e) {
		return Schedule{}, fmt.Errorf("schedule.working_hours.start (%s) must be earlier "+
			"in the day than schedule.working_hours.end (%s)", s, e)
	}
	return Schedule{Enabled: true, Start: s, End: e, Days: days, Loc: loc}, nil
}

// Disabled builds a schedule with no working-hours window. The timezone is still
// loaded so times can be formatted in the user's locale.
func Disabled(timezone string) (Schedule, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule.timezone %q: %w", timezone, err)
	}
	return Schedule{Enabled: false, Loc: loc}, nil
}

// on returns the absolute instant of DayTime dt on the local date of t.
func (sc Schedule) on(t time.Time, dt DayTime) time.Time {
	y, m, d := t.In(sc.Loc).Date()
	return time.Date(y, m, d, dt.Hour, dt.Minute, 0, 0, sc.Loc)
}

// activeDay reports whether t's local weekday is one of the configured days. An
// empty Days set means every day is active.
func (sc Schedule) activeDay(t time.Time) bool {
	if len(sc.Days) == 0 {
		return true
	}
	return slices.Contains(sc.Days, t.In(sc.Loc).Weekday())
}

// WithinWorkingHours reports whether t is inside the protected window [start, end)
// on an active weekday of its local date. False when working hours are disabled.
func (sc Schedule) WithinWorkingHours(t time.Time) bool {
	if !sc.Enabled || !sc.activeDay(t) {
		return false
	}
	start := sc.on(t, sc.Start)
	end := sc.on(t, sc.End)
	return !t.Before(start) && t.Before(end)
}

// NextWorkingHoursStart returns the next window start strictly after t on an
// active weekday. ok is false when working hours are disabled.
func (sc Schedule) NextWorkingHoursStart(t time.Time) (time.Time, bool) {
	if !sc.Enabled {
		return time.Time{}, false
	}
	// Today plus the following seven local days always contains an active day's
	// start strictly after t (an empty Days set means every day is active).
	// Stepping the local calendar date keeps DST-length days from skipping one.
	local := t.In(sc.Loc)
	for i := range 8 {
		cand := sc.on(local.AddDate(0, 0, i), sc.Start)
		if cand.After(t) && sc.activeDay(cand) {
			return cand, true
		}
	}
	return time.Time{}, false
}

// Decision reasons, the machine-readable half of a Decision.
const (
	ReasonScheduledSleep = "scheduled-sleep"
	ReasonKeepAwake      = "keep-awake"
	ReasonWorkingHours   = "working-hours"
	ReasonIdleDisabled   = "idle-disabled"
	ReasonActive         = "active"
	ReasonBootGrace      = "boot-grace"
	ReasonNoActivity     = "no-activity"
	ReasonIdle           = "idle"
)

// Decision is the reconciler's suspend verdict, with a machine-readable reason.
type Decision struct {
	// Suspend is true when a RUNNING VM should be suspended now.
	Suspend bool
	// Reason explains the verdict; one of the Reason* constants.
	Reason string
}

// Activity is what idle shutdown measures from. Either time is zero when
// unknown.
type Activity struct {
	// LastActive is the last time a herdr agent was working or an inbound SSH
	// connection was open, as reported by the on-VM emitter.
	LastActive time.Time
	// LastStart is the instance's last start (Compute's lastStartTimestamp), so
	// a VM booted by anything — first provisioning, a replacement, a Console
	// start — gets a full idle timeout before it can be suspended.
	LastStart time.Time
}

// MaxActivitySkew bounds how far in the future a reported last-active time may
// lie (VM clock drift) before it is ignored as bogus: anything on the VM can
// write the guest attribute, and a far-future value must not disable idle
// shutdown.
const MaxActivitySkew = 5 * time.Minute

// FutureActivity reports whether a last-active time lies further ahead of now
// than MaxActivitySkew, i.e. is too far in the future to trust. AutoSuspend
// ignores such a value; status and doctor use the same rule to explain why.
func FutureActivity(lastActive, now time.Time) bool {
	return lastActive.After(now.Add(MaxActivitySkew))
}

// AutoSuspend decides whether the reconciler should suspend a RUNNING VM at now.
// It never wakes: waking is always a manual action. idleTimeout of zero disables
// idle shutdown.
//
// Precedence: a one-off scheduled sleep forces suspend even within working hours;
// a keep-awake hold inhibits suspend; working hours inhibit suspend; otherwise
// idle shutdown applies, counting from the later of act.LastActive and
// act.LastStart. With neither known it fails safe toward suspending, so an
// emitter that stops reporting cannot keep the VM awake.
func (sc Schedule) AutoSuspend(now time.Time, spans Spans, act Activity, idleTimeout time.Duration) Decision {
	if spans.Sleep.Active(now) && spans.Sleep.State == Asleep {
		return Decision{Suspend: true, Reason: ReasonScheduledSleep}
	}
	if spans.Hold.Active(now) && spans.Hold.State == Awake {
		return Decision{Suspend: false, Reason: ReasonKeepAwake}
	}
	if sc.WithinWorkingHours(now) {
		return Decision{Suspend: false, Reason: ReasonWorkingHours}
	}
	if idleTimeout <= 0 {
		return Decision{Suspend: false, Reason: ReasonIdleDisabled}
	}
	lastActive := act.LastActive
	if FutureActivity(lastActive, now) {
		lastActive = time.Time{}
	}
	if !lastActive.IsZero() && now.Sub(lastActive) < idleTimeout {
		return Decision{Suspend: false, Reason: ReasonActive}
	}
	if !act.LastStart.IsZero() && now.Sub(act.LastStart) < idleTimeout {
		return Decision{Suspend: false, Reason: ReasonBootGrace}
	}
	if lastActive.IsZero() {
		return Decision{Suspend: true, Reason: ReasonNoActivity}
	}
	return Decision{Suspend: true, Reason: ReasonIdle}
}

// KeepAwakeHold builds an Awake hold from now for the given duration. It backs
// both `keep-awake` and the short grace window `wake` establishes so a freshly
// woken VM is not suspended before activity is first reported.
func (sc Schedule) KeepAwakeHold(now time.Time, d time.Duration) Span {
	return Span{Start: now, End: now.Add(d), State: Awake}
}

// nextOccurrence returns the next instant of dt at or after now.
func (sc Schedule) nextOccurrence(now time.Time, dt DayTime) time.Time {
	cand := sc.on(now, dt)
	if cand.Before(now) {
		// Step one calendar day in the schedule timezone, not the caller's, so a
		// DST-length day in another zone can't skip a date.
		cand = sc.on(now.In(sc.Loc).AddDate(0, 0, 1), dt)
	}
	return cand
}

// maxScheduledSleep bounds a scheduled sleep when there is no working-hours
// window to end it: long enough to cover a night, short enough not to linger.
const maxScheduledSleep = 8 * time.Hour

// ScheduledSleep builds a one-off sleep span forcing Asleep at the next
// occurrence of dt at or after now. It lasts until the next working-hours start
// on an active day (which may be days away when `days` is restricted), or
// start+maxScheduledSleep when working hours are disabled; a manual `wake` while
// it is in effect cancels it (see cli.App.Wake).
func (sc Schedule) ScheduledSleep(now time.Time, dt DayTime) Span {
	start := sc.nextOccurrence(now, dt)
	end, ok := sc.NextWorkingHoursStart(start)
	if !ok {
		end = start.Add(maxScheduledSleep)
	}
	return Span{Start: start, End: end, State: Asleep}
}
