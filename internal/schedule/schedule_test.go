package schedule

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func mustSchedule(t *testing.T) Schedule {
	t.Helper()
	sc, err := New("06:00", "23:00", "Europe/Stockholm")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sc
}

func mustDisabled(t *testing.T) Schedule {
	t.Helper()
	sc, err := Disabled("Europe/Stockholm")
	if err != nil {
		t.Fatalf("Disabled: %v", err)
	}
	return sc
}

// testYear is the year every test instant falls in; the month and day carry
// the meaning (weekday, DST side), so repeating it at each call site adds
// nothing.
const testYear = 2026

// at builds an instant in the schedule's timezone for the given date and time.
func at(t *testing.T, sc Schedule, m time.Month, d, h, minute int) time.Time {
	t.Helper()
	return time.Date(testYear, m, d, h, minute, 0, 0, sc.Loc)
}

func TestParseDayTime(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    DayTime
		wantErr bool
	}{
		{"06:00", DayTime{6, 0}, false},
		{"23:30", DayTime{23, 30}, false},
		{"00:00", DayTime{0, 0}, false},
		{"24:00", DayTime{}, true},
		{"12:60", DayTime{}, true},
		{"noon", DayTime{}, true},
		{"", DayTime{}, true},
	} {
		got, err := ParseDayTime(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDayTime(%q): want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDayTime(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseDayTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The errors name full config key paths so a user can grep their YAML for them
// (see the ledger decision).
func TestNewValidates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   func() error
		wantKey string
	}{
		{"start after end", func() error { _, err := New("23:00", "06:00", "Europe/Stockholm"); return err },
			"schedule.working_hours.start"},
		{"bad start", func() error { _, err := New("nope", "23:00", "Europe/Stockholm"); return err },
			"schedule.working_hours.start"},
		{"bad end", func() error { _, err := New("06:00", "nope", "Europe/Stockholm"); return err },
			"schedule.working_hours.end"},
		{"bad timezone", func() error { _, err := New("06:00", "23:00", "Nowhere/Nowhere"); return err },
			"schedule.timezone"},
		{"bad timezone, disabled", func() error { _, err := Disabled("Nowhere/Nowhere"); return err },
			"schedule.timezone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error = %v, want it to name %s", err, tc.wantKey)
			}
		})
	}
}

func TestWithinWorkingHours(t *testing.T) {
	sc := mustSchedule(t)
	for _, tc := range []struct {
		h, m int
		want bool
	}{
		{0, 0, false},
		{5, 59, false},
		{6, 0, true},
		{12, 0, true},
		{22, 59, true},
		{23, 0, false},
		{23, 30, false},
	} {
		got := sc.WithinWorkingHours(at(t, sc, time.June, 15, tc.h, tc.m))
		if got != tc.want {
			t.Errorf("WithinWorkingHours(%02d:%02d) = %v, want %v", tc.h, tc.m, got, tc.want)
		}
	}
	// A disabled schedule is never within working hours.
	dis := mustDisabled(t)
	if dis.WithinWorkingHours(at(t, dis, time.June, 15, 12, 0)) {
		t.Error("disabled schedule should never be within working hours")
	}
}

func TestWorkingHoursWeekdaysOnly(t *testing.T) {
	// 07:00–17:00 on weekdays only.
	sc, err := New("07:00", "17:00", "Europe/Stockholm",
		time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-06-15 is a Monday; 2026-06-13 a Saturday, 2026-06-14 a Sunday.
	if !sc.WithinWorkingHours(at(t, sc, time.June, 15, 12, 0)) {
		t.Error("Monday noon should be within working hours")
	}
	if sc.WithinWorkingHours(at(t, sc, time.June, 13, 12, 0)) {
		t.Error("Saturday noon should be outside working hours (weekday-only)")
	}
	if sc.WithinWorkingHours(at(t, sc, time.June, 14, 12, 0)) {
		t.Error("Sunday noon should be outside working hours (weekday-only)")
	}
	// Outside the time window on a weekday is also outside.
	if sc.WithinWorkingHours(at(t, sc, time.June, 15, 18, 0)) {
		t.Error("Monday 18:00 is past the window")
	}
}

func TestAutoSuspendWeekend(t *testing.T) {
	sc, err := New("07:00", "17:00", "Europe/Stockholm",
		time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday)
	if err != nil {
		t.Fatal(err)
	}
	// Saturday noon, idle past timeout: no working-hours protection → suspend.
	sat := at(t, sc, time.June, 13, 12, 0)
	if got := sc.AutoSuspend(sat, Spans{}, Activity{LastActive: sat.Add(-time.Hour)}, 30*time.Minute); !got.Suspend || got.Reason != ReasonIdle {
		t.Errorf("weekend idle: got %+v, want suspend idle", got)
	}
	// Monday noon, idle: protected by working hours.
	mon := at(t, sc, time.June, 15, 12, 0)
	if got := sc.AutoSuspend(mon, Spans{}, Activity{LastActive: mon.Add(-time.Hour)}, 30*time.Minute); got.Suspend || got.Reason != ReasonWorkingHours {
		t.Errorf("weekday within hours: got %+v, want no-suspend working-hours", got)
	}
}

func TestAutoSuspend(t *testing.T) {
	sc := mustSchedule(t)
	const idle = 30 * time.Minute

	// Reference instants.
	noon := at(t, sc, time.June, 15, 12, 0)   // within working hours
	night := at(t, sc, time.June, 15, 23, 30) // outside working hours

	holdAwake := &Span{Start: night.Add(-time.Hour), End: night.Add(time.Hour), State: Awake}
	holdNoon := &Span{Start: noon.Add(-time.Hour), End: noon.Add(time.Hour), State: Awake}
	sleepNow := &Span{Start: noon.Add(-time.Minute), End: noon.Add(time.Hour), State: Asleep}
	sleepNight := &Span{Start: night.Add(-time.Minute), End: night.Add(time.Hour), State: Asleep}
	// Spans whose State doesn't match their slot (e.g. left over from an older
	// wire model) must be ignored.
	holdAsleep := &Span{Start: night.Add(-time.Hour), End: night.Add(time.Hour), State: Asleep}
	sleepAwake := &Span{Start: noon.Add(-time.Minute), End: noon.Add(time.Hour), State: Awake}

	for _, tc := range []struct {
		name        string
		now         time.Time
		spans       Spans
		act         Activity
		idle        time.Duration
		wantSuspend bool
		wantReason  string
	}{
		{
			name: "within working hours, idle, still protected",
			now:  noon, act: Activity{LastActive: noon.Add(-2 * time.Hour)}, idle: idle,
			wantSuspend: false, wantReason: ReasonWorkingHours,
		},
		{
			name: "one-off scheduled sleep beats working hours",
			now:  noon, spans: Spans{Sleep: sleepNow}, act: Activity{LastActive: noon}, idle: idle,
			wantSuspend: true, wantReason: ReasonScheduledSleep,
		},
		{
			name: "one-off scheduled sleep beats a keep-awake hold",
			now:  noon, spans: Spans{Sleep: sleepNow, Hold: holdNoon}, act: Activity{LastActive: noon}, idle: idle,
			wantSuspend: true, wantReason: ReasonScheduledSleep,
		},
		{
			name: "scheduled sleep with awake state is ignored",
			now:  noon, spans: Spans{Sleep: sleepAwake}, act: Activity{LastActive: noon}, idle: idle,
			wantSuspend: false, wantReason: ReasonWorkingHours,
		},
		{
			name: "keep-awake hold inhibits suspend outside hours",
			now:  night, spans: Spans{Hold: holdAwake}, act: Activity{LastActive: night.Add(-2 * time.Hour)}, idle: idle,
			wantSuspend: false, wantReason: ReasonKeepAwake,
		},
		{
			name: "hold with asleep state is ignored",
			now:  night, spans: Spans{Hold: holdAsleep}, act: Activity{LastActive: night.Add(-2 * time.Hour)}, idle: idle,
			wantSuspend: true, wantReason: ReasonIdle,
		},
		{
			name: "scheduled sleep outside hours beats recent activity",
			now:  night, spans: Spans{Sleep: sleepNight}, act: Activity{LastActive: night}, idle: idle,
			wantSuspend: true, wantReason: ReasonScheduledSleep,
		},
		{
			name: "outside hours, recently active",
			now:  night, act: Activity{LastActive: night.Add(-10 * time.Minute)}, idle: idle,
			wantSuspend: false, wantReason: ReasonActive,
		},
		{
			name: "outside hours, idle past timeout",
			now:  night, act: Activity{LastActive: night.Add(-31 * time.Minute)}, idle: idle,
			wantSuspend: true, wantReason: ReasonIdle,
		},
		{
			name: "outside hours, idle exactly at timeout",
			now:  night, act: Activity{LastActive: night.Add(-idle)}, idle: idle,
			wantSuspend: true, wantReason: ReasonIdle,
		},
		{
			name: "outside hours, idle shutdown disabled",
			now:  night, act: Activity{LastActive: night.Add(-2 * time.Hour)}, idle: 0,
			wantSuspend: false, wantReason: ReasonIdleDisabled,
		},
		{
			name: "outside hours, nothing known, fails safe to suspend",
			now:  night, idle: idle,
			wantSuspend: true, wantReason: ReasonNoActivity,
		},
		{
			name: "freshly booted, no activity yet: boot grace",
			now:  night, act: Activity{LastStart: night.Add(-5 * time.Minute)}, idle: idle,
			wantSuspend: false, wantReason: ReasonBootGrace,
		},
		{
			name: "booted after the last activity: boot grace",
			now:  night, act: Activity{LastActive: night.Add(-2 * time.Hour), LastStart: night.Add(-5 * time.Minute)}, idle: idle,
			wantSuspend: false, wantReason: ReasonBootGrace,
		},
		{
			name: "boot grace exactly at timeout, no activity",
			now:  night, act: Activity{LastStart: night.Add(-idle)}, idle: idle,
			wantSuspend: true, wantReason: ReasonNoActivity,
		},
		{
			name: "boot grace over, stale activity from before the boot",
			now:  night, act: Activity{LastActive: night.Add(-3 * time.Hour), LastStart: night.Add(-time.Hour)}, idle: idle,
			wantSuspend: true, wantReason: ReasonIdle,
		},
		{
			name: "activity slightly in the future (clock skew) counts as active",
			now:  night, act: Activity{LastActive: night.Add(MaxActivitySkew)}, idle: idle,
			wantSuspend: false, wantReason: ReasonActive,
		},
		{
			name: "far-future activity is ignored",
			now:  night, act: Activity{LastActive: night.Add(MaxActivitySkew + time.Second)}, idle: idle,
			wantSuspend: true, wantReason: ReasonNoActivity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sc.AutoSuspend(tc.now, tc.spans, tc.act, tc.idle)
			if got.Suspend != tc.wantSuspend || got.Reason != tc.wantReason {
				t.Errorf("AutoSuspend = %+v, want {Suspend:%v Reason:%q}", got, tc.wantSuspend, tc.wantReason)
			}
		})
	}
}

// TestSpanBoundaries pins the half-open [Start, End) semantics the reconciler
// mirrors (startS <= now and now < endS).
func TestSpanBoundaries(t *testing.T) {
	start := time.Date(2026, time.June, 15, 20, 0, 0, 0, time.UTC)
	sp := &Span{Start: start, End: start.Add(time.Hour), State: Awake}
	if !sp.Active(sp.Start) {
		t.Error("span should be active at its start")
	}
	if sp.Active(sp.Start.Add(-time.Nanosecond)) {
		t.Error("span should not be active before its start")
	}
	if sp.Active(sp.End) {
		t.Error("span should not be active at its end")
	}
	if !sp.Expired(sp.End) {
		t.Error("span should be expired at its end")
	}
	if sp.Expired(sp.End.Add(-time.Nanosecond)) {
		t.Error("span should not be expired just before its end")
	}
}

func TestAutoSuspendDisabledWorkingHours(t *testing.T) {
	sc := mustDisabled(t)
	const idle = 30 * time.Minute
	now := at(t, sc, time.June, 15, 12, 0)
	// With no working hours, midday behaves like any other time: idle governs.
	if got := sc.AutoSuspend(now, Spans{}, Activity{LastActive: now.Add(-time.Hour)}, idle); !got.Suspend || got.Reason != ReasonIdle {
		t.Errorf("disabled+idle: got %+v, want suspend idle", got)
	}
	if got := sc.AutoSuspend(now, Spans{}, Activity{LastActive: now.Add(-time.Minute)}, idle); got.Suspend || got.Reason != ReasonActive {
		t.Errorf("disabled+active: got %+v, want no-suspend active", got)
	}
}

func TestKeepAwakeHold(t *testing.T) {
	sc := mustSchedule(t)
	now := at(t, sc, time.June, 15, 23, 30)
	sp := sc.KeepAwakeHold(now, 2*time.Hour)
	if sp.State != Awake || !sp.End.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("KeepAwakeHold = %+v, want awake for 2h", sp)
	}
	if !sp.Active(now.Add(time.Hour)) {
		t.Error("hold should be active within its window")
	}
	if sp.Active(now.Add(3 * time.Hour)) {
		t.Error("hold should have expired after its window")
	}
}

func TestScheduledSleepToday(t *testing.T) {
	sc := mustSchedule(t)
	// Afternoon; a one-off sleep at 20:00 should fire tonight.
	now := at(t, sc, time.June, 15, 15, 0)
	sp := sc.ScheduledSleep(now, DayTime{20, 0})
	wantStart := at(t, sc, time.June, 15, 20, 0)
	wantEnd := at(t, sc, time.June, 16, 6, 0) // next working-hours start
	if !sp.Start.Equal(wantStart) || !sp.End.Equal(wantEnd) || sp.State != Asleep {
		t.Fatalf("ScheduledSleep = %+v, want [%v,%v) asleep", sp, wantStart, wantEnd)
	}
	// Before 20:00 it is not active; from 20:00 it forces suspend.
	if sp.Active(at(t, sc, time.June, 15, 19, 0)) {
		t.Error("scheduled sleep should not be active before its time")
	}
	if got := sc.AutoSuspend(at(t, sc, time.June, 15, 20, 30), Spans{Sleep: &sp}, Activity{LastActive: now}, 30*time.Minute); !got.Suspend {
		t.Errorf("scheduled sleep should force suspend at 20:30, got %+v", got)
	}
}

// A sleep requested for the current minute starts now, not tomorrow.
func TestScheduledSleepAtCurrentTime(t *testing.T) {
	sc := mustSchedule(t)
	now := at(t, sc, time.June, 15, 20, 0)
	if sp := sc.ScheduledSleep(now, DayTime{20, 0}); !sp.Start.Equal(now) {
		t.Errorf("ScheduledSleep start = %v, want %v (now)", sp.Start, now)
	}
}

func TestScheduledSleepRollsToTomorrow(t *testing.T) {
	sc := mustSchedule(t)
	// It is already 21:00; a one-off sleep at 20:00 is in the past, so it rolls
	// to tomorrow rather than resolving backward.
	now := at(t, sc, time.June, 15, 21, 0)
	sp := sc.ScheduledSleep(now, DayTime{20, 0})
	wantStart := at(t, sc, time.June, 16, 20, 0)
	if !sp.Start.Equal(wantStart) {
		t.Errorf("ScheduledSleep start = %v, want %v (tomorrow)", sp.Start, wantStart)
	}
	if sp.Start.Before(now) {
		t.Errorf("scheduled sleep must not start in the past: %v", sp.Start)
	}
}

func TestScheduledSleepRollsOneDayInScheduleZone(t *testing.T) {
	sc := mustSchedule(t) // Europe/Stockholm
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 23:30 Stockholm on Sat 31 Oct 2026, seen from a laptop in New York the
	// evening before its DST fall-back (a 25h day there). Rolling "tomorrow" in
	// the laptop's zone would land on 2 Nov in Stockholm and skip 1 Nov.
	now := at(t, sc, time.October, 31, 23, 30).In(ny)
	sp := sc.ScheduledSleep(now, DayTime{20, 0})
	if want := at(t, sc, time.November, 1, 20, 0); !sp.Start.Equal(want) {
		t.Errorf("ScheduledSleep start = %v, want %v", sp.Start, want)
	}
	if want := at(t, sc, time.November, 2, 6, 0); !sp.End.Equal(want) {
		t.Errorf("ScheduledSleep end = %v, want %v", sp.End, want)
	}
}

func TestScheduledSleepDisabledWindow(t *testing.T) {
	sc := mustDisabled(t)
	now := at(t, sc, time.June, 15, 15, 0)
	sp := sc.ScheduledSleep(now, DayTime{20, 0})
	wantStart := at(t, sc, time.June, 15, 20, 0)
	if !sp.Start.Equal(wantStart) || !sp.End.Equal(wantStart.Add(8*time.Hour)) {
		t.Fatalf("ScheduledSleep (disabled) = %+v, want [%v,+8h)", sp, wantStart)
	}
}

// DST: Sweden springs forward 2026-03-29 (02:00 -> 03:00) and falls back
// 2026-10-25 (03:00 -> 02:00). Working-hours boundaries must land on the correct
// wall-clock times regardless.
func TestDSTSpringForward(t *testing.T) {
	sc := mustSchedule(t)
	start := sc.on(at(t, sc, time.March, 29, 12, 0), sc.Start)
	_, offset := start.Zone()
	if offset != 2*3600 {
		t.Errorf("spring-forward start offset = %d, want +7200", offset)
	}
	if !sc.WithinWorkingHours(at(t, sc, time.March, 29, 7, 0)) {
		t.Error("07:00 on DST day should be within working hours")
	}
}

func TestDSTFallBack(t *testing.T) {
	sc := mustSchedule(t)
	before := sc.on(at(t, sc, time.October, 24, 12, 0), sc.Start)
	after := sc.on(at(t, sc, time.October, 26, 12, 0), sc.Start)
	_, offBefore := before.Zone()
	_, offAfter := after.Zone()
	if offBefore != 2*3600 {
		t.Errorf("pre-fallback offset = %d, want +7200", offBefore)
	}
	if offAfter != 1*3600 {
		t.Errorf("post-fallback offset = %d, want +3600", offAfter)
	}
}

func TestNextWorkingHoursStart(t *testing.T) {
	sc := mustSchedule(t)
	// Midday inside the window: the next start is tomorrow morning.
	now := at(t, sc, time.June, 15, 12, 0)
	if start, ok := sc.NextWorkingHoursStart(now); !ok || !start.Equal(at(t, sc, time.June, 16, 6, 0)) {
		t.Errorf("NextWorkingHoursStart = %v ok=%v, want 2026-06-16 06:00", start, ok)
	}
	// Weekday-only window: from Friday evening the next start is Monday.
	wk, err := New("06:00", "23:00", "Europe/Stockholm",
		time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday)
	if err != nil {
		t.Fatal(err)
	}
	fri := at(t, wk, time.June, 19, 23, 30)
	if start, ok := wk.NextWorkingHoursStart(fri); !ok || !start.Equal(at(t, wk, time.June, 22, 6, 0)) {
		t.Errorf("NextWorkingHoursStart(Fri) = %v ok=%v, want Mon 2026-06-22 06:00", start, ok)
	}
	// Single-day window, asked after that day's start: the next start is exactly
	// seven days out — the far end of the scan.
	mon, err := New("06:00", "23:00", "Europe/Stockholm", time.Monday)
	if err != nil {
		t.Fatal(err)
	}
	monNoon := at(t, mon, time.June, 15, 12, 0)
	nextMon := at(t, mon, time.June, 22, 6, 0)
	if start, ok := mon.NextWorkingHoursStart(monNoon); !ok || !start.Equal(nextMon) {
		t.Errorf("NextWorkingHoursStart(Mon-only, Mon noon) = %v ok=%v, want %v", start, ok, nextMon)
	}
	if sp := mon.ScheduledSleep(monNoon, DayTime{20, 0}); !sp.End.Equal(nextMon) {
		t.Errorf("ScheduledSleep end (Mon-only) = %v, want %v", sp.End, nextMon)
	}
	// Disabled schedule reports no start.
	if _, ok := mustDisabled(t).NextWorkingHoursStart(now); ok {
		t.Error("disabled schedule should report no working-hours start")
	}
}

// TestReconcilerMirrorsEngine checks the reconciler template (which cannot run
// locally) against the engine it mirrors: every Reason is an action it can
// return, its clock-skew bound matches MaxActivitySkew, and its idle
// comparisons match AutoSuspend's.
func TestReconcilerMirrorsEngine(t *testing.T) {
	raw, err := os.ReadFile("../../infra/reconcile.yaml.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, r := range []string{
		ReasonScheduledSleep, ReasonKeepAwake, ReasonWorkingHours, ReasonIdleDisabled,
		ReasonActive, ReasonBootGrace, ReasonNoActivity, ReasonIdle,
	} {
		if want := `action: "` + r + `"`; !strings.Contains(src, want) {
			t.Errorf("infra/reconcile.yaml.tftpl has no %s", want)
		}
	}
	if want := fmt.Sprintf("maxSkewS: %d", int(MaxActivitySkew/time.Second)); !strings.Contains(src, want) {
		t.Errorf("infra/reconcile.yaml.tftpl has no %s (MaxActivitySkew)", want)
	}
	// The idle comparisons AutoSuspend mirrors. (The reconciler's "not-running"
	// verdict is pinned against cli.ReasonNotRunning in internal/cli.)
	for _, want := range []string{
		"now - lastActiveS < idleMin * 60",
		"now - startedS < idleMin * 60",
		// The skew clamp, the template half of schedule.FutureActivity.
		"lastActiveS > now + maxSkewS",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("infra/reconcile.yaml.tftpl no longer contains %s", want)
		}
	}
}

// assertDecisionOrder pins the reconciler's precedence: the ordered switch
// targets of each decision step, and the ordered step list of main (several
// steps carry no next: and fall through to the one listed after them, so their
// order is part of the flow). Mirrors schedule.AutoSuspend.
func assertDecisionOrder(t *testing.T, doc map[string]any) {
	t.Helper()
	steps, ok := doc["main"].(map[string]any)["steps"].([]any)
	if !ok {
		t.Fatal("main.steps is not a list; did the template layout change?")
	}
	var names []string
	byName := map[string]map[string]any{}
	for _, item := range steps {
		for name, body := range item.(map[string]any) {
			names = append(names, name)
			if m, ok := body.(map[string]any); ok {
				byName[name] = m
			}
		}
	}
	wantOrder := []string{
		"init", "getInstance", "status", "guardRunning", "notRunning", "readState",
		"fields", "evalSleep", "evalHold", "weekdayInit", "withinHours",
		"decideScheduled", "decideProtected", "keptKeepAwake", "keptWorkingHours",
		"keptIdleDisabled", "readActivity", "idleInputs", "activityScan",
		"activityHave", "activityParse", "activityClamp", "activityFuture",
		"startedPick", "startedParse", "decideIdle", "keptBootGrace", "keptActive",
		"markScheduledSleep", "markNoActivity", "markIdle", "suspendVm", "waitVmOp",
		"cleanup",
	}
	if !slices.Equal(names, wantOrder) {
		t.Fatalf("main step order =\n%v\nwant\n%v", names, wantOrder)
	}
	// The ordered switch targets encode the precedence itself.
	for step, want := range map[string][]string{
		"decideScheduled": {"markScheduledSleep"},
		"decideProtected": {"keptKeepAwake", "keptWorkingHours", "keptIdleDisabled"},
		"decideIdle":      {"keptActive", "keptBootGrace", "markNoActivity"},
	} {
		cases, ok := byName[step]["switch"].([]any)
		if !ok {
			t.Errorf("%s has no switch; did the template layout change?", step)
			continue
		}
		var got []string
		for _, c := range cases {
			got = append(got, c.(map[string]any)["next"].(string))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s switch targets = %v, want %v", step, got, want)
		}
	}
}

// TestReconcilerRendersAndRoutes renders the reconciler template with sample
// values, parses it as YAML and checks that every next: names a real step and
// every call: to a local subworkflow names a real one, so a broken route fails
// here rather than at `make tf-apply`.
func TestReconcilerRendersAndRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../infra/reconcile.yaml.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	sample := map[string]string{
		"work_start_min": "360", "work_end_min": "1380", "idle_min": "30",
		"work_days_map": `{"1": true}`,
	}
	const escaped = "\x00"
	src := strings.ReplaceAll(string(raw), "$${", escaped)
	src = regexp.MustCompile(`\$\{(\w+)\}`).ReplaceAllStringFunc(src, func(m string) string {
		if v, ok := sample[m[2:len(m)-1]]; ok {
			return v
		}
		return "sample"
	})
	src = strings.ReplaceAll(src, escaped, "${")

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("rendered reconciler is not valid YAML: %v", err)
	}
	steps := map[string]bool{}
	var nexts, calls []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				if k == "steps" {
					if list, ok := child.([]any); ok {
						for _, item := range list {
							if m, ok := item.(map[string]any); ok {
								for name := range m {
									steps[name] = true
								}
							}
						}
					}
				}
				if k == "next" {
					if s, ok := child.(string); ok {
						nexts = append(nexts, s)
					}
				}
				// The template's own subworkflows are plain identifiers; connectors
				// and standard-library calls are dotted.
				if k == "call" {
					if s, ok := child.(string); ok && !strings.Contains(s, ".") {
						calls = append(calls, s)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	assertDecisionOrder(t, doc)
	if len(nexts) == 0 {
		t.Fatal("found no next: routes; did the template layout change?")
	}
	// "end" ends the workflow; "continue"/"break" are the loop keywords.
	keywords := map[string]bool{"end": true, "continue": true, "break": true}
	for _, n := range nexts {
		if !steps[n] && !keywords[n] {
			t.Errorf("next: %q names no step", n)
		}
	}
	if len(calls) == 0 {
		t.Fatal("found no subworkflow calls; did the template layout change?")
	}
	for _, c := range calls {
		if _, ok := doc[c]; !ok {
			t.Errorf("call: %q names no subworkflow", c)
		}
	}
}
