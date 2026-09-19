package ssh

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBaseArgs(t *testing.T) {
	for _, tc := range []struct {
		target Target
		want   []string
	}{
		{Target{Host: "workbox"}, []string{"workbox"}},
		{Target{Host: "workbox", User: "developer"}, []string{"-l", "developer", "workbox"}},
	} {
		got := tc.target.baseArgs()
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("baseArgs(%+v) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

func TestWaitReachableTimeout(t *testing.T) {
	// 198.51.100.1 is an RFC 5737 documentation address (never routed).
	// interval > timeout so exactly one Reachable poll runs before deadline fires.
	target := Target{Host: "198.51.100.1"}
	err := WaitReachable(context.Background(), target, 200*time.Millisecond, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "198.51.100.1") {
		t.Errorf("error should mention host: %v", err)
	}
}

func TestWaitReachableCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := Target{Host: "198.51.100.1"}
	err := WaitReachable(ctx, target, 10*time.Second, 100*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
