package compute

import (
	"context"
	"time"
)

// Activity reports what idle shutdown measures from: the VM's last-active time
// — the most recent moment a herdr agent was working or an inbound SSH
// connection was open — as recorded by the on-VM emitter and exposed through a
// guest attribute, and the instance's last start. It is read-only and
// best-effort: it drives `workbox status` and `workbox doctor`, not the suspend
// decision (the reconciler enforces that).
type Activity interface {
	// LastActive returns the last-active time. ok is false when it is unknown
	// (VM asleep, or never reported). err wraps ErrInvalidActivity when the
	// stored value is not a usable unix timestamp (non-numeric, zero or
	// negative) — treat that as unknown; such a value never counts as recent
	// activity to the reconciler either. Any other non-nil err is a read
	// failure.
	LastActive(ctx context.Context) (t time.Time, ok bool, err error)
	// LastStart returns the instance's last start (lastStartTimestamp). ok is
	// false — and t is the zero time — when the instance has never started. A
	// non-nil err is a read or parse failure; treat the start time as unknown.
	LastStart(ctx context.Context) (t time.Time, ok bool, err error)
}
