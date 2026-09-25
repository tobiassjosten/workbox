package compute

import (
	"context"
	"sync"
	"time"
)

// Fake is an in-memory Compute for tests. It records the operations invoked and
// can be scripted with a sequence of states returned by successive Status calls
// to simulate transitional progressions.
type Fake struct {
	mu sync.Mutex

	// Current state; also updated by Start/Resume/Suspend and by Status
	// when consuming StatusSeq.
	Current State
	// StatusSeq, when non-empty, is consumed one entry per Status call, after
	// which Current is returned.
	StatusSeq []State

	Calls []string
	// Err, when non-nil, is returned by Start/Resume/Suspend once ErrSeq is
	// exhausted. Status succeeds unless StatusErr is set, so tests can drive a
	// failing transition from a known state.
	Err error
	// ErrSeq, when non-empty, is consumed one entry per Start/Resume/Suspend
	// call and takes precedence over Err, so tests can drive an operation that
	// fails a few times and then succeeds. A nil entry means success.
	ErrSeq []error
	// StatusErr, when non-nil, is returned by Status.
	StatusErr error
}

// NewFake returns a Fake starting in the given state.
func NewFake(s State) *Fake { return &Fake{Current: s} }

func (f *Fake) Status(_ context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "status")
	if f.StatusErr != nil {
		return Unknown, f.StatusErr
	}
	if len(f.StatusSeq) > 0 {
		s := f.StatusSeq[0]
		f.StatusSeq = f.StatusSeq[1:]
		f.Current = s
		return s, nil
	}
	return f.Current, nil
}

func (f *Fake) Start(_ context.Context) error   { return f.op("start", Running) }
func (f *Fake) Resume(_ context.Context) error  { return f.op("resume", Running) }
func (f *Fake) Suspend(_ context.Context) error { return f.op("suspend", Suspended) }

func (f *Fake) op(name string, result State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, name)
	err := f.Err
	if len(f.ErrSeq) > 0 {
		err = f.ErrSeq[0]
		f.ErrSeq = f.ErrSeq[1:]
	}
	// A failed operation leaves the fake's state alone. The real instance may
	// land elsewhere — a capacity-failed resume can leave it TERMINATED — which
	// tests model by scripting StatusSeq alongside ErrSeq.
	if err != nil {
		return err
	}
	f.Current = result
	return nil
}

// FakeActivity is an in-memory Activity for tests. ActiveAt/ActiveOK are what
// LastActive reports and LastStartAt what LastStart reports; ActiveErr and
// LastStartErr are the matching failures. A zero LastStartAt reports the
// instance as never started.
type FakeActivity struct {
	ActiveAt     time.Time
	ActiveOK     bool
	ActiveErr    error
	LastStartAt  time.Time
	LastStartErr error
}

// LastActive returns the scripted values. As with the real client, an error
// comes with no value.
func (f FakeActivity) LastActive(_ context.Context) (time.Time, bool, error) {
	if f.ActiveErr != nil {
		return time.Time{}, false, f.ActiveErr
	}
	return f.ActiveAt, f.ActiveOK, nil
}

// LastStart returns the scripted start time.
func (f FakeActivity) LastStart(_ context.Context) (time.Time, bool, error) {
	if f.LastStartErr != nil {
		return time.Time{}, false, f.LastStartErr
	}
	return f.LastStartAt, !f.LastStartAt.IsZero(), nil
}

var (
	_ Compute  = (*Fake)(nil)
	_ Activity = FakeActivity{}
)
