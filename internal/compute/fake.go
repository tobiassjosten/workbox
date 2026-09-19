package compute

import (
	"context"
	"sync"
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
	// Err, when non-nil, is returned by Start/Resume/Suspend. Status always
	// succeeds so tests can drive a failing transition from a known state.
	Err error
}

// NewFake returns a Fake starting in the given state.
func NewFake(s State) *Fake { return &Fake{Current: s} }

func (f *Fake) Status(_ context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "status")
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
	if f.Err != nil {
		return f.Err
	}
	f.Current = result
	return nil
}

var _ Compute = (*Fake)(nil)
