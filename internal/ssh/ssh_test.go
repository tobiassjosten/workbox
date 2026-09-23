package ssh

import (
	"context"
	"errors"
	"reflect"
	"strconv"
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

func TestRemoteArgs(t *testing.T) {
	got := RemoteArgs(Target{Host: "workbox", User: "developer"}, 5*time.Second, "true")
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ForwardAgent=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-l", "developer", "workbox", "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RemoteArgs = %v, want %v", got, want)
	}
}

// ssh treats ConnectTimeout=0 as "no timeout", so a sub-second value must round
// up to 1 rather than down to 0.
func TestRemoteArgsClampsSubSecondTimeout(t *testing.T) {
	got := RemoteArgs(Target{Host: "workbox"}, 100*time.Millisecond, "true")
	if got[3] != "ConnectTimeout=1" {
		t.Errorf("RemoteArgs(100ms) connect timeout = %q, want %q", got[3], "ConnectTimeout=1")
	}
}

// The probe's stderr sink must consume everything it is given (a short count
// makes os/exec fail the command) while keeping only the last bytes, which is
// where ssh's own diagnostic lands.
func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{limit: 16}
	big := strings.Repeat("x", 100)
	if n, err := b.Write([]byte(big)); n != len(big) || err != nil {
		t.Fatalf("Write = %d, %v, want %d, nil", n, err, len(big))
	}
	if got, want := b.String(), strings.Repeat("x", 16); got != want {
		t.Errorf("String() = %q, want the last %d bytes", got, len(want))
	}
	if n, err := b.Write([]byte("Permission denied")); n != 17 || err != nil {
		t.Fatalf("Write = %d, %v, want 17, nil", n, err)
	}
	if got := b.String(); got != "ermission denied" {
		t.Errorf("String() = %q, want the tail", got)
	}
}

func TestProbeErr(t *testing.T) {
	runErr := errors.New("exit status 255")
	for _, tc := range []struct {
		name   string
		runErr error
		stderr string
		want   string
	}{
		{"success", nil, "Warning: Permanently added 'workbox'", ""},
		{"no stderr", runErr, "", "exit status 255"},
		{"last line wins", runErr, "Warning: Permanently added 'workbox'\ndeveloper@workbox: Permission denied (publickey).\n",
			`"developer@workbox: Permission denied (publickey)." (exit status 255)`},
		// A hostile VM's banner reaches this string; control bytes must not
		// reach the terminal raw.
		{"control characters escaped", runErr, "EVIL\x1b[2K banner",
			`"EVIL\x1b[2K banner" (exit status 255)`},
		// The cap counts runes, so a multi-byte banner is never cut mid-rune.
		{"long multi-byte line is truncated on a rune boundary", runErr, strings.Repeat("ä", 250),
			strconv.Quote(strings.Repeat("ä", 200)+"…") + " (exit status 255)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := probeErr(tc.runErr, tc.stderr)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("probeErr = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("probeErr = %v, want %q", err, tc.want)
			}
			if !errors.Is(err, runErr) {
				t.Errorf("probeErr should wrap the run error")
			}
		})
	}
}

func TestWaitReachableTimeout(t *testing.T) {
	// 198.51.100.1 is an RFC 5737 documentation address (never routed).
	// interval > timeout so exactly one probe runs before the deadline fires.
	target := Target{Host: "198.51.100.1"}
	err := WaitReachable(context.Background(), target, 100*time.Millisecond, 200*time.Millisecond, 500*time.Millisecond)
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

// The timeout error carries the last probe error that finished on its own; a
// probe killed by the deadline says nothing about the cause and is dropped.
func TestWaitReachableKeepsLastProbeError(t *testing.T) {
	target := Target{Host: "workbox"}
	denied := errors.New("Permission denied (publickey)")
	for _, tc := range []struct {
		name string
		try  func(context.Context) error
		want string
	}{
		{
			name: "fast failure, then killed by the deadline",
			try: func() func(context.Context) error {
				calls := 0
				return func(ctx context.Context) error {
					calls++
					if calls == 1 {
						return denied
					}
					<-ctx.Done()
					return errors.New("signal: killed")
				}
			}(),
			want: `ssh to "workbox" not reachable within 50ms (last error: Permission denied (publickey)): context deadline exceeded`,
		},
		{
			name: "only ever killed by the deadline",
			try: func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("signal: killed")
			},
			want: `ssh to "workbox" not reachable within 50ms: context deadline exceeded`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := waitReachable(context.Background(), target, 50*time.Millisecond, time.Millisecond, tc.try)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("waitReachable = %v, want %q", err, tc.want)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error should wrap context.DeadlineExceeded")
			}
		})
	}
}

func TestWaitReachableCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := Target{Host: "198.51.100.1"}
	err := WaitReachable(ctx, target, 5*time.Second, 10*time.Second, 100*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
