package compute

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestParseState(t *testing.T) {
	if ParseState("RUNNING") != Running {
		t.Error("RUNNING")
	}
	if ParseState("STOPPED") != Unknown {
		t.Error("GCP has no STOPPED; want Unknown")
	}
	if !Suspending.IsTransitional() || Suspending.IsStable() {
		t.Error("Suspending should be transitional, not stable")
	}
	if !Running.IsStable() {
		t.Error("Running should be stable")
	}
}

func TestWakeIdempotentWhenRunning(t *testing.T) {
	f := NewFake(Running)
	if err := Wake(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status"}) {
		t.Errorf("calls = %v, want just status", f.Calls)
	}
}

func TestWakeResumesSuspended(t *testing.T) {
	f := NewFake(Suspended)
	if err := Wake(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status", "resume"}) {
		t.Errorf("calls = %v, want status,resume", f.Calls)
	}
	if f.Current != Running {
		t.Errorf("state = %v, want Running", f.Current)
	}
}

func TestWakeStartsTerminated(t *testing.T) {
	f := NewFake(Terminated)
	if err := Wake(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status", "start"}) {
		t.Errorf("calls = %v, want status,start", f.Calls)
	}
}

func TestWakeWaitsOutTransitional(t *testing.T) {
	f := &Fake{StatusSeq: []State{Suspending, Suspended}}
	if err := Wake(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	// status(Suspending) -> WaitStable status(Suspended) -> loop status(Suspended) -> resume
	want := []string{"status", "status", "status", "resume"}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Errorf("calls = %v, want %v", f.Calls, want)
	}
}

func TestSleepIdempotentWhenSuspended(t *testing.T) {
	f := NewFake(Suspended)
	if err := Sleep(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status"}) {
		t.Errorf("calls = %v, want just status", f.Calls)
	}
}

func TestSleepSuspendsRunning(t *testing.T) {
	f := NewFake(Running)
	if err := Sleep(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status", "suspend"}) {
		t.Errorf("calls = %v, want status,suspend", f.Calls)
	}
}

func TestSleepIdempotentWhenTerminated(t *testing.T) {
	f := NewFake(Terminated)
	if err := Sleep(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status"}) {
		t.Errorf("calls = %v, want just status", f.Calls)
	}
}

func TestWakeRepairingErrors(t *testing.T) {
	f := NewFake(Repairing)
	err := Wake(context.Background(), f)
	if !errors.Is(err, ErrUnsupportedState) {
		t.Errorf("err = %v, want ErrUnsupportedState", err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status"}) {
		t.Errorf("calls = %v, want just status (no power op on REPAIRING)", f.Calls)
	}
}

func TestWaitStableUnexpectedStateErrors(t *testing.T) {
	// A transitional state that settles into an unrecognized (Unknown) status must
	// terminate with ErrUnsupportedState, not poll the stuck state indefinitely.
	f := &Fake{StatusSeq: []State{Staging, Unknown}}
	err := Wake(context.Background(), f)
	if !errors.Is(err, ErrUnsupportedState) {
		t.Errorf("err = %v, want ErrUnsupportedState", err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status", "status"}) {
		t.Errorf("calls = %v, want status,status (no indefinite polling)", f.Calls)
	}
}

func TestSleepRepairingErrors(t *testing.T) {
	f := NewFake(Repairing)
	err := Sleep(context.Background(), f)
	if !errors.Is(err, ErrUnsupportedState) {
		t.Errorf("err = %v, want ErrUnsupportedState", err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"status"}) {
		t.Errorf("calls = %v, want just status (no power op on REPAIRING)", f.Calls)
	}
}

func TestContextCancellationDuringWait(t *testing.T) {
	f := &Fake{Current: Suspending} // never settles
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wake(ctx, f); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
