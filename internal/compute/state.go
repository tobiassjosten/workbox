package compute

// State is a normalized Compute Engine instance state. It maps the raw GCP
// status strings onto an explicit set the rest of the program reasons about,
// so raw strings never leak beyond this package.
type State string

const (
	// Running: the instance is up (GCP RUNNING).
	Running State = "RUNNING"
	// Suspended: memory/device state preserved on disk (GCP SUSPENDED).
	Suspended State = "SUSPENDED"
	// Terminated: stopped, no preserved memory (GCP TERMINATED).
	Terminated State = "TERMINATED"
	// Provisioning: resources being allocated.
	Provisioning State = "PROVISIONING"
	// Staging: booting toward RUNNING.
	Staging State = "STAGING"
	// Stopping: on the way to TERMINATED.
	Stopping State = "STOPPING"
	// Suspending: on the way to SUSPENDED.
	Suspending State = "SUSPENDING"
	// Repairing: the instance is being repaired and is not usable.
	Repairing State = "REPAIRING"
	// Unknown: an unrecognized status.
	Unknown State = "UNKNOWN"
)

// ParseState normalizes a raw GCP status string.
func ParseState(raw string) State {
	switch State(raw) {
	case Running, Suspended, Terminated, Provisioning, Staging, Stopping, Suspending, Repairing:
		return State(raw)
	default:
		return Unknown
	}
}

// IsTransitional reports whether the state is a transient state that will
// settle into a stable one without intervention (Provisioning, Staging,
// Stopping, or Suspending).
func (s State) IsTransitional() bool {
	switch s {
	case Provisioning, Staging, Stopping, Suspending:
		return true
	default:
		return false
	}
}

// IsStable reports whether the state is a settled state.
func (s State) IsStable() bool {
	switch s {
	case Running, Suspended, Terminated:
		return true
	default:
		return false
	}
}

// String returns the raw state string.
func (s State) String() string { return string(s) }
