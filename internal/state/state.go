// Package state stores the tiny operational documents that drive auto-suspend:
// a one-off scheduled sleep and the keep-awake hold. The same document is read by
// the GCP Workflow reconciler, so the field layout here is the contract with
// infra/reconcile.yaml.tftpl.
package state

import (
	"context"
	"time"

	"github.com/tobiassjosten/workbox/internal/schedule"
)

// Kind identifies which slot a span occupies. The values happen to match the
// Firestore field names, but the wire contract lives in firestore.go's struct
// tags.
type Kind string

const (
	// KindSleep is the one-off scheduled sleep.
	KindSleep Kind = "scheduled_sleep"
	// KindHold is the keep-awake hold (also the post-wake grace window).
	KindHold Kind = "hold"
)

// Document is the operational-state document. Each span is optional.
type Document struct {
	Sleep     *schedule.Span
	Hold      *schedule.Span
	UpdatedBy string
	UpdatedAt time.Time
}

// Spans returns the schedule.Spans view of the document.
func (d *Document) Spans() schedule.Spans {
	if d == nil {
		return schedule.Spans{}
	}
	return schedule.Spans{Sleep: d.Sleep, Hold: d.Hold}
}

func copySpan(s *schedule.Span) *schedule.Span {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// activeSpan returns a copy of s unless it has expired at t (a nil span counts
// as expired) or its State is not want, the state its slot requires; then it
// returns nil. Copying only the survivors keeps the pruning free of throwaway
// allocations.
func activeSpan(s *schedule.Span, t time.Time, want schedule.SpanState) *schedule.Span {
	if s.Expired(t) || s.State != want {
		return nil
	}
	return copySpan(s)
}

// Active returns the document with any spans that have fully expired at t
// removed, so callers never present stale spans. A span whose state doesn't fit
// its slot is dropped too, since the reconciler and AutoSuspend ignore it and it
// must not be shown as in effect: an asleep hold is the shape the
// pre-working-hours model could leave behind, and an awake scheduled sleep can
// only come from malformed data.
func (d *Document) Active(t time.Time) *Document {
	if d == nil {
		return &Document{} // empty but non-nil; callers can dereference safely
	}
	out := *d
	out.Sleep = activeSpan(d.Sleep, t, schedule.Asleep)
	out.Hold = activeSpan(d.Hold, t, schedule.Awake)
	return &out
}

// Set places span in the given slot.
func (d *Document) Set(kind Kind, span schedule.Span) {
	switch kind {
	case KindSleep:
		d.Sleep = &span
	case KindHold:
		d.Hold = &span
	}
}

// Empty reports whether the document holds no spans — the condition that
// decides whether it is worth saving or should be deleted.
func (d *Document) Empty() bool {
	return d.Sleep == nil && d.Hold == nil
}

// Store persists the operational-state document.
type Store interface {
	// Load returns the current document, or an empty document if none exists.
	Load(ctx context.Context) (*Document, error)
	// Save writes the document, replacing any existing one.
	Save(ctx context.Context, doc *Document) error
	// Clear removes the scheduled sleep and keep-awake hold.
	Clear(ctx context.Context) error
}
