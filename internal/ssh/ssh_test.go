package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
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

func TestCheckLocalPorts(t *testing.T) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	busy := ln.Addr().(*net.TCPAddr).Port
	if err := CheckLocalPorts([]Forward{{Local: busy, Remote: 80}}); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("cannot bind local port %d", busy)) {
		t.Errorf("CheckLocalPorts(busy) = %v, want a cannot-bind error", err)
	}
	// Once released, the same port is free again.
	_ = ln.Close()
	if err := CheckLocalPorts([]Forward{{Local: busy, Remote: 80}}); err != nil {
		t.Errorf("CheckLocalPorts(free) = %v, want nil", err)
	}
}

// ssh -L binds every address localhost resolves to, so a port held on either
// loopback family must be reported busy.
func TestCheckLocalPortsBothFamilies(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			ln, err := net.Listen("tcp", host+":0")
			if err != nil {
				t.Skipf("cannot listen on %s: %v", host, err)
			}
			defer func() { _ = ln.Close() }()
			busy := ln.Addr().(*net.TCPAddr).Port
			if err := CheckLocalPorts([]Forward{{Local: busy, Remote: 80}}); err == nil {
				t.Errorf("CheckLocalPorts = nil, want a busy error for a port held on %s", host)
			}
		})
	}
}

func TestForwardArgv(t *testing.T) {
	got := forwardArgv(Target{Host: "workbox"}, []Forward{{Local: 8080, Remote: 1313}})
	want := []string{
		"ssh", "-N", "-o", "ExitOnForwardFailure=yes", "-o", "ForwardAgent=no",
		"-o", "ControlMaster=no",
		"-L", "localhost:8080:localhost:1313", "workbox",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forwardArgv = %v, want %v", got, want)
	}
}

func TestParseForward(t *testing.T) {
	for _, tc := range []struct {
		spec    string
		want    Forward
		wantArg string
	}{
		{"1313", Forward{Local: 1313, Remote: 1313}, "localhost:1313:localhost:1313"},
		{"8080:1313", Forward{Local: 8080, Remote: 1313}, "localhost:8080:localhost:1313"},
	} {
		got, err := parseForward(tc.spec)
		if err != nil {
			t.Errorf("parseForward(%q) unexpected error: %v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseForward(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
		if got.Arg() != tc.wantArg {
			t.Errorf("parseForward(%q).Arg() = %q, want %q", tc.spec, got.Arg(), tc.wantArg)
		}
	}
}

func TestParseForwardInvalid(t *testing.T) {
	for _, spec := range []string{"", "abc", "0", "70000", "8080:", "8080:abc", "1:2:3"} {
		if _, err := parseForward(spec); err == nil {
			t.Errorf("parseForward(%q) expected error, got nil", spec)
		}
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

func TestParseForwardsRejectsDuplicateLocalPort(t *testing.T) {
	if _, err := ParseForwards([]string{"1313", "1313:8080"}); err == nil {
		t.Error("expected an error for a local port forwarded twice")
	}
	got, err := ParseForwards([]string{"1313", "8080:80"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Forward{{Local: 1313, Remote: 1313}, {Local: 8080, Remote: 80}}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseForwards = %v, want %v", got, want)
	}
}
