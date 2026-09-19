package schedule

import (
	"testing"
	"time"
)

func mustSchedule(t *testing.T) Schedule {
	t.Helper()
	sc, err := New("06:00", "23:00", "Europe/Stockholm")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sc
}

// at builds a local Stockholm instant for the given date and time.
func at(t *testing.T, sc Schedule, y int, m time.Month, d, h, min int) time.Time {
	t.Helper()
	return time.Date(y, m, d, h, min, 0, 0, sc.Loc)
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

func TestNewValidatesOrder(t *testing.T) {
	if _, err := New("23:00", "06:00", "Europe/Stockholm"); err == nil {
		t.Error("New: expected error when wake is after sleep")
	}
	if _, err := New("06:00", "23:00", "Nowhere/Nowhere"); err == nil {
		t.Error("New: expected error for bad timezone")
	}
}

func TestBaselineNormalDay(t *testing.T) {
	sc := mustSchedule(t)
	for _, tc := range []struct {
		h, m int
		want Desired
	}{
		{0, 0, Asleep},
		{5, 59, Asleep},
		{6, 0, Awake},
		{12, 0, Awake},
		{22, 59, Awake},
		{23, 0, Asleep},
		{23, 30, Asleep},
	} {
		got := sc.Baseline(at(t, sc, 2026, time.June, 15, tc.h, tc.m))
		if got != tc.want {
			t.Errorf("Baseline(%02d:%02d) = %v, want %v", tc.h, tc.m, got, tc.want)
		}
	}
}

func TestNextTransitionNormalDay(t *testing.T) {
	sc := mustSchedule(t)
	now := at(t, sc, 2026, time.June, 15, 9, 0) // awake
	tr, ok := sc.NextTransition(now, Overrides{})
	if !ok {
		t.Fatal("expected a transition")
	}
	want := at(t, sc, 2026, time.June, 15, 23, 0)
	if !tr.At.Equal(want) || tr.To != Asleep {
		t.Errorf("NextTransition = %v -> %v, want %v -> asleep", tr.At, tr.To, want)
	}
}

func TestEarlyWakeOverride(t *testing.T) {
	sc := mustSchedule(t)
	// Evening; next baseline wake is tomorrow 06:00. Request 05:00.
	now := at(t, sc, 2026, time.June, 15, 20, 0)
	sp, ok := sc.WakeOverride(now, DayTime{5, 0})
	if !ok {
		t.Fatal("WakeOverride returned ok=false unexpectedly")
	}
	wantStart := at(t, sc, 2026, time.June, 16, 5, 0)
	wantEnd := at(t, sc, 2026, time.June, 16, 6, 0)
	if !sp.Start.Equal(wantStart) || !sp.End.Equal(wantEnd) || sp.State != Awake {
		t.Fatalf("early wake span = %+v, want [%v,%v) awake", sp, wantStart, wantEnd)
	}
	ov := Overrides{Wake: &sp}
	// At 05:30 the VM should be awake because of the override.
	if got := sc.Desired(at(t, sc, 2026, time.June, 16, 5, 30), ov); got != Awake {
		t.Errorf("Desired at 05:30 = %v, want awake", got)
	}
	// Future day unaffected: at 05:30 two days later, still asleep.
	if got := sc.Desired(at(t, sc, 2026, time.June, 17, 5, 30), ov); got != Asleep {
		t.Errorf("Desired next day 05:30 = %v, want asleep", got)
	}
}

func TestLateWakeOverride(t *testing.T) {
	sc := mustSchedule(t)
	now := at(t, sc, 2026, time.June, 15, 20, 0)
	sp, ok := sc.WakeOverride(now, DayTime{8, 30})
	if !ok {
		t.Fatal("WakeOverride returned ok=false unexpectedly")
	}
	wantStart := at(t, sc, 2026, time.June, 16, 6, 0)
	wantEnd := at(t, sc, 2026, time.June, 16, 8, 30)
	if !sp.Start.Equal(wantStart) || !sp.End.Equal(wantEnd) || sp.State != Asleep {
		t.Fatalf("late wake span = %+v, want [%v,%v) asleep", sp, wantStart, wantEnd)
	}
	ov := Overrides{Wake: &sp}
	// At 07:00 the VM should still be asleep (delayed wake).
	if got := sc.Desired(at(t, sc, 2026, time.June, 16, 7, 0), ov); got != Asleep {
		t.Errorf("Desired at 07:00 = %v, want asleep", got)
	}
	// At 09:00 it is awake.
	if got := sc.Desired(at(t, sc, 2026, time.June, 16, 9, 0), ov); got != Awake {
		t.Errorf("Desired at 09:00 = %v, want awake", got)
	}
}

func TestWakeOverrideFarInputResolvesForward(t *testing.T) {
	sc := mustSchedule(t)
	// Late evening; next baseline wake is tomorrow 06:00. A far-from-transition
	// request (20:00) must resolve forward, never to a span that starts in the
	// past or is already active now (which would force an unexpected state).
	now := at(t, sc, 2026, time.June, 15, 22, 0)
	sp, ok := sc.WakeOverride(now, DayTime{20, 0})
	if !ok {
		t.Fatal("WakeOverride returned ok=false unexpectedly")
	}
	if sp.Start.Before(now) {
		t.Errorf("span starts in the past: start=%v now=%v", sp.Start, now)
	}
	if sp.Active(now) {
		t.Errorf("override should not be active at now=%v: span=%+v", now, sp)
	}
	// Resolves to the next 20:00 (tomorrow), delaying the 06:00 baseline wake.
	wantEnd := at(t, sc, 2026, time.June, 16, 20, 0)
	if !sp.End.Equal(wantEnd) || sp.State != Asleep {
		t.Errorf("far wake span = %+v, want end %v asleep", sp, wantEnd)
	}
}

func TestWakeOverrideNoOpWhenAtBaseline(t *testing.T) {
	sc := mustSchedule(t)
	// Requesting the exact baseline wake time (06:00) yields no override.
	now := at(t, sc, 2026, time.June, 15, 20, 0)
	if _, ok := sc.WakeOverride(now, DayTime{6, 0}); ok {
		t.Error("WakeOverride at baseline wake time should return ok=false")
	}
}

func TestSleepOverrideNoOpWhenAtBaseline(t *testing.T) {
	sc := mustSchedule(t)
	// Requesting the exact baseline sleep time (23:00) yields no override.
	now := at(t, sc, 2026, time.June, 15, 15, 0)
	if _, ok := sc.SleepOverride(now, DayTime{23, 0}); ok {
		t.Error("SleepOverride at baseline sleep time should return ok=false")
	}
}

func TestEarlySleepOverride(t *testing.T) {
	sc := mustSchedule(t)
	// Afternoon; next baseline sleep is tonight 23:00. Request 20:00.
	now := at(t, sc, 2026, time.June, 15, 15, 0)
	sp, ok := sc.SleepOverride(now, DayTime{20, 0})
	if !ok {
		t.Fatal("SleepOverride returned ok=false unexpectedly")
	}
	wantStart := at(t, sc, 2026, time.June, 15, 20, 0)
	wantEnd := at(t, sc, 2026, time.June, 15, 23, 0)
	if !sp.Start.Equal(wantStart) || !sp.End.Equal(wantEnd) || sp.State != Asleep {
		t.Fatalf("early sleep span = %+v, want [%v,%v) asleep", sp, wantStart, wantEnd)
	}
	ov := Overrides{Sleep: &sp}
	if got := sc.Desired(at(t, sc, 2026, time.June, 15, 21, 0), ov); got != Asleep {
		t.Errorf("Desired at 21:00 = %v, want asleep", got)
	}
	// Future day unaffected.
	if got := sc.Desired(at(t, sc, 2026, time.June, 16, 21, 0), ov); got != Awake {
		t.Errorf("Desired next day 21:00 = %v, want awake", got)
	}
}

func TestLateSleepOverridePastMidnight(t *testing.T) {
	sc := mustSchedule(t)
	// Evening; extend tonight past midnight to 01:30 tomorrow.
	now := at(t, sc, 2026, time.June, 15, 21, 0)
	sp, ok := sc.SleepOverride(now, DayTime{1, 30})
	if !ok {
		t.Fatal("SleepOverride returned ok=false unexpectedly")
	}
	wantStart := at(t, sc, 2026, time.June, 15, 23, 0)
	wantEnd := at(t, sc, 2026, time.June, 16, 1, 30)
	if !sp.Start.Equal(wantStart) || !sp.End.Equal(wantEnd) || sp.State != Awake {
		t.Fatalf("late sleep span = %+v, want [%v,%v) awake", sp, wantStart, wantEnd)
	}
	ov := Overrides{Sleep: &sp}
	// 00:30 after midnight: still awake because of the override.
	if got := sc.Desired(at(t, sc, 2026, time.June, 16, 0, 30), ov); got != Awake {
		t.Errorf("Desired at 00:30 = %v, want awake", got)
	}
	// 02:00: override elapsed, back to baseline asleep.
	if got := sc.Desired(at(t, sc, 2026, time.June, 16, 2, 0), ov); got != Asleep {
		t.Errorf("Desired at 02:00 = %v, want asleep", got)
	}
}

func TestCurrentTimeAfterMidnightSleepOverride(t *testing.T) {
	sc := mustSchedule(t)
	// It is already 00:30, having stayed up; the override runs to 01:30.
	now := at(t, sc, 2026, time.June, 16, 0, 30)
	sp := Span{
		Start: at(t, sc, 2026, time.June, 15, 23, 0),
		End:   at(t, sc, 2026, time.June, 16, 1, 30),
		State: Awake,
	}
	ov := Overrides{Sleep: &sp}
	if got := sc.Desired(now, ov); got != Awake {
		t.Errorf("Desired at 00:30 = %v, want awake", got)
	}
	tr, ok := sc.NextTransition(now, ov)
	if !ok || tr.To != Asleep || !tr.At.Equal(sp.End) {
		t.Errorf("NextTransition = %+v ok=%v, want asleep at %v", tr, ok, sp.End)
	}
}

func TestOverrideExpiry(t *testing.T) {
	sc := mustSchedule(t)
	sp := Span{
		Start: at(t, sc, 2026, time.June, 15, 20, 0),
		End:   at(t, sc, 2026, time.June, 15, 23, 0),
		State: Asleep,
	}
	if sp.Expired(at(t, sc, 2026, time.June, 15, 22, 0)) {
		t.Error("span should not be expired at 22:00")
	}
	if !sp.Expired(at(t, sc, 2026, time.June, 15, 23, 0)) {
		t.Error("span should be expired at its end")
	}
	// An expired override does not affect a later evaluation.
	ov := Overrides{Sleep: &sp}
	if got := sc.Desired(at(t, sc, 2026, time.June, 16, 21, 0), ov); got != Awake {
		t.Errorf("Desired after expiry = %v, want awake", got)
	}
}

func TestManualWakeHold(t *testing.T) {
	sc := mustSchedule(t)
	// 01:00, baseline asleep. `workbox wake` should establish a hold.
	now := at(t, sc, 2026, time.June, 15, 1, 0)
	sp, ok := sc.WakeHold(now, Overrides{})
	if !ok {
		t.Fatal("expected a hold when baseline is asleep")
	}
	// Hold runs until the next scheduled sleep (23:00 today).
	wantEnd := at(t, sc, 2026, time.June, 15, 23, 0)
	if !sp.End.Equal(wantEnd) || sp.State != Awake {
		t.Fatalf("wake hold = %+v, want end %v awake", sp, wantEnd)
	}
	ov := Overrides{Hold: &sp}
	if got := sc.Desired(at(t, sc, 2026, time.June, 15, 3, 0), ov); got != Awake {
		t.Errorf("Desired at 03:00 with hold = %v, want awake", got)
	}
	// No hold needed when already awake.
	if _, ok := sc.WakeHold(at(t, sc, 2026, time.June, 15, 10, 0), Overrides{}); ok {
		t.Error("no hold should be needed at 10:00")
	}
}

func TestManualSleepHold(t *testing.T) {
	sc := mustSchedule(t)
	// 22:00, baseline awake. `workbox sleep` should hold asleep until next wake.
	now := at(t, sc, 2026, time.June, 15, 22, 0)
	sp, ok := sc.SleepHold(now, Overrides{})
	if !ok {
		t.Fatal("expected a hold when baseline is awake")
	}
	wantEnd := at(t, sc, 2026, time.June, 16, 6, 0)
	if !sp.End.Equal(wantEnd) || sp.State != Asleep {
		t.Fatalf("sleep hold = %+v, want end %v asleep", sp, wantEnd)
	}
	ov := Overrides{Hold: &sp}
	if got := sc.Desired(at(t, sc, 2026, time.June, 15, 22, 30), ov); got != Asleep {
		t.Errorf("Desired at 22:30 with hold = %v, want asleep", got)
	}
	if _, ok := sc.SleepHold(at(t, sc, 2026, time.June, 15, 2, 0), Overrides{}); ok {
		t.Error("no hold should be needed at 02:00")
	}
}

func TestHoldExpiry(t *testing.T) {
	sc := mustSchedule(t)
	now := at(t, sc, 2026, time.June, 15, 1, 0)
	sp := sc.KeepAwakeHold(now, 3*time.Hour)
	ov := Overrides{Hold: &sp}
	if got := sc.Desired(at(t, sc, 2026, time.June, 15, 3, 0), ov); got != Awake {
		t.Errorf("Desired inside keep-awake = %v, want awake", got)
	}
	// After the hold expires (04:00), baseline (still <06:00) is asleep.
	if got := sc.Desired(at(t, sc, 2026, time.June, 15, 5, 0), ov); got != Asleep {
		t.Errorf("Desired after keep-awake = %v, want asleep", got)
	}
}

func TestHoldBeatsOverride(t *testing.T) {
	sc := mustSchedule(t)
	now := at(t, sc, 2026, time.June, 15, 20, 0)
	// Early-sleep override says asleep at 21:00...
	sleep := Span{
		Start: at(t, sc, 2026, time.June, 15, 20, 0),
		End:   at(t, sc, 2026, time.June, 15, 23, 0),
		State: Asleep,
	}
	// ...but a keep-awake hold overrides it.
	hold := sc.KeepAwakeHold(now, 2*time.Hour)
	ov := Overrides{Sleep: &sleep, Hold: &hold}
	if got := sc.Desired(at(t, sc, 2026, time.June, 15, 21, 0), ov); got != Awake {
		t.Errorf("Desired at 21:00 = %v, want awake (hold beats override)", got)
	}
}

// DST: Sweden springs forward 2026-03-29 (02:00 -> 03:00) and falls back
// 2026-10-25 (03:00 -> 02:00). Baseline wake/sleep must land on the correct
// wall-clock times regardless.
func TestDSTSpringForward(t *testing.T) {
	sc := mustSchedule(t)
	// On the spring-forward day, 06:00 exists and is CEST (+02:00).
	wake := sc.on(at(t, sc, 2026, time.March, 29, 12, 0), sc.Wake)
	_, offset := wake.Zone()
	if offset != 2*3600 {
		t.Errorf("spring-forward wake offset = %d, want +7200", offset)
	}
	if got := sc.Baseline(at(t, sc, 2026, time.March, 29, 7, 0)); got != Awake {
		t.Errorf("Baseline 07:00 on DST day = %v, want awake", got)
	}
}

func TestDSTFallBack(t *testing.T) {
	sc := mustSchedule(t)
	// The day before fall-back is still CEST; the day after is CET (+01:00).
	before := sc.on(at(t, sc, 2026, time.October, 24, 12, 0), sc.Wake)
	after := sc.on(at(t, sc, 2026, time.October, 26, 12, 0), sc.Wake)
	_, offBefore := before.Zone()
	_, offAfter := after.Zone()
	if offBefore != 2*3600 {
		t.Errorf("pre-fallback offset = %d, want +7200", offBefore)
	}
	if offAfter != 1*3600 {
		t.Errorf("post-fallback offset = %d, want +3600", offAfter)
	}
	// A sleep override from 23:00 on the 25th to 01:30 on the 26th: both ends
	// are still CEST (the repeated hour is 02:00-03:00, after 01:30), so the
	// real elapsed time equals the wall-clock 2h30m.
	now := at(t, sc, 2026, time.October, 25, 21, 0)
	sp, _ := sc.SleepOverride(now, DayTime{1, 30})
	if got := sp.End.Sub(sp.Start); got != 2*time.Hour+30*time.Minute {
		t.Errorf("fall-back span (pre-repeat) duration = %v, want 2h30m", got)
	}
	// A span that crosses the 03:00 fall-back on the 25th picks up the extra
	// real hour: 01:00 CEST to 04:00 CET is 3 wall-clock hours but 4 real hours.
	crossStart := time.Date(2026, time.October, 25, 1, 0, 0, 0, sc.Loc)
	crossEnd := time.Date(2026, time.October, 25, 4, 0, 0, 0, sc.Loc)
	if got := crossEnd.Sub(crossStart); got != 4*time.Hour {
		t.Errorf("fall-back crossing duration = %v, want 4h real", got)
	}
}

func TestScheduleQueryHelpers(t *testing.T) {
	sc := mustSchedule(t)
	now := at(t, sc, 2026, time.June, 15, 12, 0)
	nw, ok := sc.NextWake(now, Overrides{})
	if !ok || !nw.Equal(at(t, sc, 2026, time.June, 16, 6, 0)) {
		t.Errorf("NextWake = %v ok=%v, want 2026-06-16 06:00", nw, ok)
	}
	ns, ok := sc.NextSleep(now, Overrides{})
	if !ok || !ns.Equal(at(t, sc, 2026, time.June, 15, 23, 0)) {
		t.Errorf("NextSleep = %v ok=%v, want 2026-06-15 23:00", ns, ok)
	}
}
