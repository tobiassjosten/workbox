// Package compute controls the workbox Compute Engine instance through the
// Google Cloud Go client libraries. It deliberately models a small set of
// states and operations behind the Compute interface so command and reconcile
// logic can be unit-tested without touching GCP.
package compute

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Compute is the minimal instance-control surface the CLI needs.
type Compute interface {
	// Status returns the current normalized instance state.
	Status(ctx context.Context) (State, error)
	// Start boots a TERMINATED instance and waits for the operation. It may fail
	// with ErrNoCapacity (a *CapacityError) when the zone cannot place the
	// instance's machine type; that is transient, and callers may retry.
	Start(ctx context.Context) error
	// Resume resumes a SUSPENDED instance and waits for the operation. As with
	// Start, it may fail with ErrNoCapacity.
	Resume(ctx context.Context) error
	// Suspend suspends a RUNNING instance and waits for the operation. It cannot
	// fail with ErrNoCapacity: suspending releases capacity rather than asking
	// for it.
	Suspend(ctx context.Context) error
}

// ErrUnsupportedState indicates the instance is in a state from which the
// requested transition cannot be made.
var ErrUnsupportedState = errors.New("instance is in a state that does not support this operation")

const defaultPollInterval = 3 * time.Second

// WaitStable polls until the instance reaches a stable state or ctx is done. It
// returns ErrUnsupportedState immediately if the instance is REPAIRING or in any
// other non-transitional, non-stable state (e.g. an unrecognized status mapped to
// Unknown), rather than polling such a stuck state indefinitely.
func WaitStable(ctx context.Context, c Compute) error {
	for {
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		if s == Repairing {
			return fmt.Errorf("%w: instance is REPAIRING", ErrUnsupportedState)
		}
		if s.IsStable() {
			return nil
		}
		if !s.IsTransitional() {
			return fmt.Errorf("%w: unexpected state %s", ErrUnsupportedState, s)
		}
		if err := wait(ctx, defaultPollInterval); err != nil {
			return err
		}
	}
}

// Wake brings the instance to RUNNING, choosing resume vs start by state, and
// is idempotent when the instance is already running. Transitional states are
// waited out before acting. Either path may fail with ErrNoCapacity (a
// *CapacityError) when the zone cannot place the instance's machine type; that
// is transient, and callers may retry.
func Wake(ctx context.Context, c Compute) error {
	for {
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		switch s {
		case Running:
			return nil
		case Suspended:
			return c.Resume(ctx)
		case Terminated:
			return c.Start(ctx)
		case Repairing:
			// The default branch below already rejects REPAIRING as non-transitional;
			// this case exists only to give a repair-specific "wait and retry" hint.
			return fmt.Errorf("%w: instance is REPAIRING; wait and retry", ErrUnsupportedState)
		default:
			if !s.IsTransitional() {
				return fmt.Errorf("%w: unexpected state %s", ErrUnsupportedState, s)
			}
			if err := WaitStable(ctx, c); err != nil {
				return err
			}
		}
	}
}

// Sleep suspends the instance and is idempotent when it is already suspended or
// terminated. Transitional states are waited out before acting.
func Sleep(ctx context.Context, c Compute) error {
	for {
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		switch s {
		case Suspended, Terminated:
			return nil
		case Running:
			return c.Suspend(ctx)
		case Repairing:
			return fmt.Errorf("%w: instance is REPAIRING; cannot suspend", ErrUnsupportedState)
		default:
			if !s.IsTransitional() {
				return fmt.Errorf("%w: unexpected state %s", ErrUnsupportedState, s)
			}
			if err := WaitStable(ctx, c); err != nil {
				return err
			}
		}
	}
}

// wait blocks for d or until ctx is done.
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
