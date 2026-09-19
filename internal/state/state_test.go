package state

import (
	"context"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/schedule"
)

func TestActivePrunesExpired(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	doc := &Document{
		Wake: &schedule.Span{
			Start: now.Add(-2 * time.Hour),
			End:   now.Add(-time.Hour), // expired
			State: schedule.Awake,
		},
		Hold: &schedule.Span{
			Start: now.Add(-time.Hour),
			End:   now.Add(time.Hour), // active
			State: schedule.Awake,
		},
	}
	a := doc.Active(now)
	if a.Wake != nil {
		t.Error("expired wake override should be pruned")
	}
	if a.Hold == nil {
		t.Error("active hold should be kept")
	}
}

func TestSetAndOverrides(t *testing.T) {
	doc := &Document{}
	sp := schedule.Span{State: schedule.Awake}
	doc.Set(KindHold, sp)
	if doc.Hold == nil || doc.Overrides().Hold == nil {
		t.Fatal("Set(KindHold) did not populate Hold")
	}
	if doc.Empty() {
		t.Error("Empty should be false after Set")
	}
}

func TestFakeRoundTrip(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	doc, err := f.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Empty() {
		t.Error("new fake should be empty")
	}
	doc.Set(KindWake, schedule.Span{State: schedule.Awake})
	if err := f.Save(ctx, doc); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Load(ctx)
	if got.Wake == nil {
		t.Error("saved wake override not loaded back")
	}
	if err := f.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = f.Load(ctx)
	if !got.Empty() {
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
