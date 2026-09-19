// Package state stores the tiny operational documents that drive scheduling:
// one-workday overrides and the manual hold. The same document is read by the
// GCP Workflow reconciler, so the field layout here is the contract with
// infra/reconcile.yaml.tftpl.
package state

import (
	"context"
	"time"

	"github.com/tobiassjosten/workbox/internal/schedule"
)

// Kind identifies which override slot a span occupies.
type Kind string

const (
	// KindWake is the one-workday wake override.
	KindWake Kind = "wake_override"
	// KindSleep is the one-workday sleep override.
	KindSleep Kind = "sleep_override"
	// KindHold is the manual desired-state hold.
	KindHold Kind = "hold"
)

// Document is the operational-state document. Each span is optional.
type Document struct {
	Wake      *schedule.Span
	Sleep     *schedule.Span
	Hold      *schedule.Span
	UpdatedBy string
	UpdatedAt time.Time
}

// Overrides returns the schedule.Overrides view of the document.
func (d *Document) Overrides() schedule.Overrides {
	if d == nil {
		return schedule.Overrides{}
	}
	return schedule.Overrides{Wake: d.Wake, Sleep: d.Sleep, Hold: d.Hold}
}

func copySpan(s *schedule.Span) *schedule.Span {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// activeSpan returns a copy of s if it is still active at t, or nil if it has
// expired (Span.Expired treats a nil span as expired). Copying only the
// survivors keeps the pruning free of throwaway allocations.
func activeSpan(s *schedule.Span, t time.Time) *schedule.Span {
	if s.Expired(t) {
		return nil
	}
	return copySpan(s)
}

// Active returns the document with any spans that have fully expired at t
// removed, so callers never present stale overrides.
func (d *Document) Active(t time.Time) *Document {
	if d == nil {
		return &Document{} // empty but non-nil; callers can dereference safely
	}
	out := *d
	out.Wake = activeSpan(d.Wake, t)
	out.Sleep = activeSpan(d.Sleep, t)
	out.Hold = activeSpan(d.Hold, t)
	return &out
}

// Set places span in the given slot.
func (d *Document) Set(kind Kind, span schedule.Span) {
	switch kind {
	case KindWake:
		d.Wake = &span
	case KindSleep:
		d.Sleep = &span
	case KindHold:
		d.Hold = &span
	}
}

// Empty reports whether no spans are set.
func (d *Document) Empty() bool {
	return d.Wake == nil && d.Sleep == nil && d.Hold == nil
}

// Store persists the operational-state document.
type Store interface {
	// Load returns the current document, or an empty document if none exists.
	Load(ctx context.Context) (*Document, error)
	// Save writes the document, replacing any existing one.
	Save(ctx context.Context, doc *Document) error
	// Clear removes all overrides and holds.
	Clear(ctx context.Context) error
}
