package state

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/schedule"
)

func TestActivePrunesExpired(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	doc := &Document{
		Sleep: &schedule.Span{
			Start: now.Add(-2 * time.Hour),
			End:   now.Add(-time.Hour), // expired
			State: schedule.Asleep,
		},
		Hold: &schedule.Span{
			Start: now.Add(-time.Hour),
			End:   now.Add(time.Hour), // active
			State: schedule.Awake,
		},
	}
	a := doc.Active(now)
	if a.Sleep != nil {
		t.Error("expired scheduled sleep should be pruned")
	}
	if a.Hold == nil {
		t.Error("active hold should be kept")
	}
}

func TestActiveDropsMismatchedState(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	live := func(st schedule.SpanState) *schedule.Span {
		return &schedule.Span{Start: now.Add(-time.Hour), End: now.Add(time.Hour), State: st}
	}
	// An asleep hold (left behind by the pre-working-hours model) and an awake
	// scheduled sleep (malformed data): both must be dropped.
	a := (&Document{Hold: live(schedule.Asleep), Sleep: live(schedule.Awake)}).Active(now)
	if a.Hold != nil || a.Sleep != nil {
		t.Errorf("mismatched-state spans should be dropped, got hold=%+v sleep=%+v", a.Hold, a.Sleep)
	}
}

func TestSetAndSpans(t *testing.T) {
	doc := &Document{}
	if !doc.Empty() {
		t.Error("a fresh document should be empty")
	}
	sp := schedule.Span{State: schedule.Awake}
	doc.Set(KindHold, sp)
	if doc.Hold == nil || doc.Spans().Hold == nil {
		t.Fatal("Set(KindHold) did not populate Hold")
	}
	if doc.Empty() {
		t.Error("a document with a hold is not empty")
	}
	sleepOnly := &Document{}
	sleepOnly.Set(KindSleep, schedule.Span{State: schedule.Asleep})
	if sleepOnly.Empty() {
		t.Error("a document with a scheduled sleep is not empty")
	}
	if sleepOnly.Spans().Sleep == nil {
		t.Error("Spans() did not carry the scheduled sleep")
	}
}

func TestFakeRoundTrip(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	doc, err := f.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Sleep != nil || doc.Hold != nil {
		t.Error("new fake should be empty")
	}
	doc.Set(KindSleep, schedule.Span{State: schedule.Asleep})
	if err := f.Save(ctx, doc); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Load(ctx)
	if got.Sleep == nil {
		t.Error("saved scheduled sleep not loaded back")
	}
	if err := f.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = f.Load(ctx)
	if got.Sleep != nil || got.Hold != nil {
		t.Error("Clear did not empty the store")
	}
}

func TestFakeSaveIsolatesStoredDoc(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	wantEnd := time.Date(2026, 6, 15, 23, 0, 0, 0, time.UTC)
	doc := &Document{Hold: &schedule.Span{
		Start: time.Date(2026, 6, 15, 20, 0, 0, 0, time.UTC),
		End:   wantEnd,
		State: schedule.Awake,
	}}
	if err := f.Save(ctx, doc); err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's span in place after Save must not reach the stored
	// copy — the fake deep-copies to match Firestore serialise/deserialise isolation.
	doc.Hold.State = schedule.Asleep
	doc.Hold.End = wantEnd.Add(24 * time.Hour)

	got, _ := f.Load(ctx)
	if got.Hold == nil {
		t.Fatal("saved hold not loaded back")
	}
	if got.Hold.State != schedule.Awake || !got.Hold.End.Equal(wantEnd) {
		t.Errorf("stored hold = {state:%v end:%v}, want {Awake %v}; deep-copy isolation broken",
			got.Hold.State, got.Hold.End, wantEnd)
	}
}

// The span state strings are compared literally by the reconciler
// (infra/reconcile.yaml.tftpl: `sSleep == "asleep"`, `sHold == "awake"`), so
// renaming them here means renaming them there in the same change.
func TestSpanStateWireValues(t *testing.T) {
	if got := string(schedule.Asleep); got != "asleep" {
		t.Errorf("schedule.Asleep = %q, want %q (see infra/reconcile.yaml.tftpl)", got, "asleep")
	}
	if got := string(schedule.Awake); got != "awake" {
		t.Errorf("schedule.Awake = %q, want %q (see infra/reconcile.yaml.tftpl)", got, "awake")
	}
}

// The Firestore field names are the wire contract with the reconciler
// (infra/reconcile.yaml.tftpl); renaming one here means renaming it there in the
// same change. This pins them so a one-sided rename fails the build.
func TestWireFieldNames(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want map[string]string
	}{
		{reflect.TypeOf(wireDoc{}), map[string]string{
			"Sleep": "scheduled_sleep", "Hold": "hold",
			"UpdatedBy": "updated_by", "UpdatedAt": "updated_at",
		}},
		{reflect.TypeOf(wireSpan{}), map[string]string{
			"Start": "start", "End": "end", "State": "state",
		}},
	} {
		t.Run(tc.typ.Name(), func(t *testing.T) {
			if got := tc.typ.NumField(); got != len(tc.want) {
				t.Fatalf("%s has %d fields, want %d — pin the new field's tag here and in the reconciler", tc.typ.Name(), got, len(tc.want))
			}
			for name, want := range tc.want {
				f, ok := tc.typ.FieldByName(name)
				if !ok {
					t.Errorf("field %s is gone", name)
					continue
				}
				if got := f.Tag.Get("firestore"); got != want {
					t.Errorf("%s firestore tag = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// TestReconcilerReadsWireNames checks the other side of the contract: the
// reconciler template must read the same field names and state values pinned
// above, so a rename made only in the template fails too.
func TestReconcilerReadsWireNames(t *testing.T) {
	raw, err := os.ReadFile("../../infra/reconcile.yaml.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		`key: "scheduled_sleep"`,
		`key: "hold"`,
		`sp.start.timestampValue`,
		`sp.end.timestampValue`,
		`sp.state.stringValue`,
		`sSleep == "` + string(schedule.Asleep) + `"`,
		`sHold == "` + string(schedule.Awake) + `"`,
		// The half-open [start, end) comparison TestSpanBoundaries pins on the
		// Go side.
		"startS <= now and now < endS",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("infra/reconcile.yaml.tftpl no longer contains %s", want)
		}
	}
}
