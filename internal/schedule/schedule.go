// Package schedule computes when the workbox VM should be awake or asleep.
//
// The model has three layers, in increasing precedence:
//
//  1. Baseline recurring schedule: awake during [wake, sleep) in local time,
//     every day, in the configured IANA timezone.
//  2. One-workday overrides (wake_override / sleep_override): a single span of
//     absolute time during which the desired state differs from the baseline.
//     These are produced by `workbox wake-at` / `workbox sleep-at` and expire
//     automatically once the span ends.
//  3. Manual hold: a single span produced by `workbox wake`, `workbox sleep`
//     and `workbox keep-awake`, with the highest precedence.
//
// Everything is expressed as absolute time.Time values, so DST transitions and
// midnight crossings are handled by the standard library's timezone database
// rather than by manual offset arithmetic. The same logic runs in the Go CLI
// (for `status`/`schedule`) and, in a simplified interval form, in the GCP
// Workflow reconciler.
package schedule

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Desired is the state the scheduler wants the VM to be in.
type Desired string

const (
	// Awake means the VM should be RUNNING. The concrete transition into this
	// state (resume vs start) is decided by compute.Wake, not here.
	Awake Desired = "awake"
	// Asleep means the VM should be SUSPENDED (or TERMINATED). The concrete
	// transition into this state is decided by compute.Sleep, not here.
	Asleep Desired = "asleep"
)

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

// Span is a half-open interval [Start, End) during which State applies.
type Span struct {
	Start time.Time
	End   time.Time
	State Desired
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

// Overrides carries the optional operational overrides read from Firestore.
type Overrides struct {
	// Wake is the one-workday override for a wake transition.
	Wake *Span
	// Sleep is the one-workday override for a sleep transition.
	Sleep *Span
	// Hold is the manual desired-state hold.
	Hold *Span
}

// Schedule is the baseline recurring schedule.
type Schedule struct {
	Wake  DayTime
	Sleep DayTime
	Loc   *time.Location
}

// New builds a Schedule from HH:MM strings and an IANA timezone name.
func New(wake, sleep, timezone string) (Schedule, error) {
	w, err := ParseDayTime(wake)
	if err != nil {
		return Schedule{}, fmt.Errorf("wake: %w", err)
	}
	s, err := ParseDayTime(sleep)
	if err != nil {
		return Schedule{}, fmt.Errorf("sleep: %w", err)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return Schedule{}, fmt.Errorf("timezone %q: %w", timezone, err)
	}
	if minutes(w) >= minutes(s) {
		return Schedule{}, fmt.Errorf("wake (%s) must be earlier in the day than sleep (%s)", w, s)
	}
	return Schedule{Wake: w, Sleep: s, Loc: loc}, nil
}

func minutes(d DayTime) int { return d.Hour*60 + d.Minute }

// on returns the absolute instant of DayTime dt on the local date of t.
func (sc Schedule) on(t time.Time, dt DayTime) time.Time {
	y, m, d := t.In(sc.Loc).Date()
	return time.Date(y, m, d, dt.Hour, dt.Minute, 0, 0, sc.Loc)
}

// prevLocalDay returns midnight of the day before t in the schedule's timezone.
// Both boundaries and nextBaselineInto start scanning from this point so that a
// wake/sleep that falls on today is not missed.
func (sc Schedule) prevLocalDay(t time.Time) time.Time {
	return sc.on(t, DayTime{}).AddDate(0, 0, -1)
}

// Baseline returns the desired state from the recurring schedule alone.
func (sc Schedule) Baseline(t time.Time) Desired {
	wake := sc.on(t, sc.Wake)
	sleep := sc.on(t, sc.Sleep)
	if !t.Before(wake) && t.Before(sleep) {
		return Awake
	}
	return Asleep
}

// Desired returns the effective desired state at t, applying overrides in
// precedence order: baseline < one-workday overrides < manual hold.
func (sc Schedule) Desired(t time.Time, ov Overrides) Desired {
	d := sc.Baseline(t)
	if ov.Wake.Active(t) {
		d = ov.Wake.State
	}
	if ov.Sleep.Active(t) {
		d = ov.Sleep.State
	}
	if ov.Hold.Active(t) {
		d = ov.Hold.State
	}
	return d
}

// Transition is a future change in desired state.
type Transition struct {
	At time.Time
	To Desired
}

// window is the default scan horizon for NextTransition and friends; the scan
// horizon is extended automatically when a span ends beyond this limit.
const window = 8 * 24 * time.Hour

// boundaries returns the sorted set of instants strictly after from and no
// later than the scan horizon where the desired state could change.
func (sc Schedule) boundaries(from time.Time, ov Overrides) []time.Time {
	end := from.Add(window)
	for _, sp := range []*Span{ov.Wake, ov.Sleep, ov.Hold} {
		if sp != nil && sp.End.After(end) {
			end = sp.End // extend horizon so this span's end will be scanned
		}
	}

	var bs []time.Time
	add := func(t time.Time) {
		if t.After(from) && !t.After(end) {
			bs = append(bs, t)
		}
	}

	// Baseline wake/sleep instants for every local date in the horizon.
	day := sc.prevLocalDay(from)
	for !day.After(end) {
		add(sc.on(day, sc.Wake))
		add(sc.on(day, sc.Sleep))
		day = day.AddDate(0, 0, 1)
	}

	// Span edges.
	for _, sp := range []*Span{ov.Wake, ov.Sleep, ov.Hold} {
		if sp == nil {
			continue
		}
		add(sp.Start)
		add(sp.End)
	}

	sort.Slice(bs, func(i, j int) bool { return bs[i].Before(bs[j]) })
	// Deduplicate.
	out := bs[:0]
	for i, t := range bs {
		if i == 0 || !t.Equal(bs[i-1]) {
			out = append(out, t)
		}
	}
	return out
}

// Transitions returns the ordered desired-state changes after from.
func (sc Schedule) Transitions(from time.Time, ov Overrides) []Transition {
	var out []Transition
	prev := sc.Desired(from, ov)
	for _, b := range sc.boundaries(from, ov) {
		s := sc.Desired(b, ov)
		if s != prev {
			out = append(out, Transition{At: b, To: s})
			prev = s
		}
	}
	return out
}

// NextTransition returns the next desired-state change after from, if any.
func (sc Schedule) NextTransition(from time.Time, ov Overrides) (Transition, bool) {
	ts := sc.Transitions(from, ov)
	if len(ts) == 0 {
		return Transition{}, false
	}
	return ts[0], true
}

// NextInto returns the next transition after from that enters the target state.
func (sc Schedule) NextInto(from time.Time, ov Overrides, target Desired) (time.Time, bool) {
	for _, t := range sc.Transitions(from, ov) {
		if t.To == target {
			return t.At, true
		}
	}
	return time.Time{}, false
}

// NextWake returns the next instant the VM should wake after from.
func (sc Schedule) NextWake(from time.Time, ov Overrides) (time.Time, bool) {
	return sc.NextInto(from, ov, Awake)
}

// NextSleep returns the next instant the VM should sleep after from.
func (sc Schedule) NextSleep(from time.Time, ov Overrides) (time.Time, bool) {
	return sc.NextInto(from, ov, Asleep)
}

// maxBaselineScanDays is the per-call bound for nextBaselineInto. Any daily
// wake/sleep transition recurs within 2 calendar days; 10 is more than enough
// for any valid schedule and makes the panic unreachable in practice. New
// enforces wake < sleep, so every schedule reaching here has a daily transition
// into each state.
const maxBaselineScanDays = 10

// nextBaselineInto returns the next baseline-only transition into target after
// from, ignoring overrides. It is used to find the baseline boundary that an
// override replaces.
func (sc Schedule) nextBaselineInto(from time.Time, target Desired) time.Time {
	day := sc.prevLocalDay(from)
	for range maxBaselineScanDays {
		var cand time.Time
		if target == Awake {
			cand = sc.on(day, sc.Wake)
		} else {
			cand = sc.on(day, sc.Sleep)
		}
		if cand.After(from) {
			return cand
		}
		day = day.AddDate(0, 0, 1)
	}
	panic(fmt.Sprintf("nextBaselineInto: no %v transition found within %d days (wake=%s, sleep=%s)", target, maxBaselineScanDays, sc.Wake, sc.Sleep))
}

// resolveNear returns the absolute instant of dt on whichever local date places
// it closest to anchor, considering only occurrences at or after notBefore. dt is
// expected to be near the targeted transition (a morning wake or an evening
// sleep), so the ±1 day search window is sufficient. Exact ties (±12h) resolve to
// the anchor's own date (day-zero wins). This makes "sleep-at 01:30" resolve to
// tomorrow when the anchor (tonight's sleep) is 23:00, and "sleep-at 20:00"
// resolve to today.
//
// The notBefore floor (the current time) keeps resolution forward-only: an
// occurrence already in the past is skipped, so a far-from-transition input never
// resolves backward into a span that starts before now and forces an unexpected
// state immediately. The day after the anchor is always at or after notBefore
// (callers pass an anchor strictly after now), so a valid candidate always exists.
func (sc Schedule) resolveNear(anchor, notBefore time.Time, dt DayTime) time.Time {
	base := sc.on(anchor, dt)
	var best time.Time
	var bestDelta time.Duration
	for _, days := range []int{0, -1, 1} { // day-zero first so it wins exact ties
		cand := time.Date(base.Year(), base.Month(), base.Day()+days, dt.Hour, dt.Minute, 0, 0, sc.Loc)
		if cand.Before(notBefore) {
			continue // forward-only: never resolve to an instant in the past
		}
		if d := absDuration(cand.Sub(anchor)); best.IsZero() || d < bestDelta {
			best, bestDelta = cand, d
		}
	}
	return best
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// WakeOverride builds a one-workday wake override for the given local HH:MM,
// relative to now. It targets the next baseline wake and returns a span that
// covers the difference between the baseline wake and the requested time.
//
//   - Earlier than baseline (e.g. 05:00 vs 06:00): span [requested, baseline)
//     forcing Awake — the VM wakes early and the baseline keeps it awake after.
//   - Later than baseline (e.g. 08:30 vs 06:00): span [baseline, requested)
//     forcing Asleep — the VM stays asleep until the requested time.
//
// Returns ok=false when the requested time equals the baseline wake (nothing to
// override).
func (sc Schedule) WakeOverride(now time.Time, dt DayTime) (Span, bool) {
	baseline := sc.nextBaselineInto(now, Awake)
	target := sc.resolveNear(baseline, now, dt)
	if target.Equal(baseline) {
		return Span{}, false
	}
	if target.Before(baseline) {
		return Span{Start: target, End: baseline, State: Awake}, true
	}
	return Span{Start: baseline, End: target, State: Asleep}, true
}

// SleepOverride builds a one-workday sleep override for the given local HH:MM,
// relative to now.
//
//   - Earlier than baseline (e.g. 20:00 vs 23:00): span [requested, baseline)
//     forcing Asleep — the VM sleeps early.
//   - Later than baseline (e.g. 01:30 vs 23:00): span [baseline, requested)
//     forcing Awake — the VM stays awake past the normal sleep, into the next
//     day if necessary.
//
// Returns ok=false when the requested time equals the baseline sleep (nothing to
// override).
func (sc Schedule) SleepOverride(now time.Time, dt DayTime) (Span, bool) {
	baseline := sc.nextBaselineInto(now, Asleep)
	target := sc.resolveNear(baseline, now, dt)
	if target.Equal(baseline) {
		return Span{}, false
	}
	if target.After(baseline) {
		return Span{Start: baseline, End: target, State: Awake}, true
	}
	return Span{Start: target, End: baseline, State: Asleep}, true
}

// WakeHold builds a manual "stay awake" hold from now until the next scheduled
// sleep, applying any active sleep override. It returns ok=false when the
// current effective desired state (including any active overrides) is already
// Awake, meaning no hold is required.
func (sc Schedule) WakeHold(now time.Time, ov Overrides) (Span, bool) {
	if sc.Desired(now, ov) == Awake {
		return Span{}, false
	}
	end, ok := sc.NextSleep(now, ov)
	if !ok {
		// Unreachable for a valid schedule; keeps any hold finite as a safeguard.
		end = now.Add(24 * time.Hour)
	}
	return Span{Start: now, End: end, State: Awake}, true
}

// SleepHold builds a manual "stay asleep" hold from now until the next
// scheduled wake. It returns ok=false when the current effective desired state
// (including any active overrides) is already Asleep, meaning no hold is required.
func (sc Schedule) SleepHold(now time.Time, ov Overrides) (Span, bool) {
	if sc.Desired(now, ov) == Asleep {
		return Span{}, false
	}
	end, ok := sc.NextWake(now, ov)
	if !ok {
		// Unreachable for a valid schedule; keeps any hold finite as a safeguard.
		end = now.Add(24 * time.Hour)
	}
	return Span{Start: now, End: end, State: Asleep}, true
}

// KeepAwakeHold builds a manual hold that keeps the machine awake for at least
// the given duration.
func (sc Schedule) KeepAwakeHold(now time.Time, d time.Duration) Span {
	return Span{Start: now, End: now.Add(d), State: Awake}
}
