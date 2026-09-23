// Package ssh reaches the workbox over the user's real OpenSSH client. It never
// implements an SSH stack in Go: connectivity checks and interactive sessions
// both run the system `ssh`, so the user inherits ~/.ssh/config, ssh-agent,
// known_hosts, ProxyJump and ControlMaster exactly as usual (the non-interactive
// probes opt out of multiplexing entirely; see RemoteArgs).
package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Target describes how to reach the workbox.
type Target struct {
	// Host is the SSH destination (a Host alias in ~/.ssh/config or a MagicDNS
	// name), e.g. "workbox".
	Host string
	// User is the login user. When set and Host carries no user, it is passed
	// with -l so `workbox ssh` works even without an ~/.ssh/config entry.
	User string
}

// Destination returns "user@host" when a user is set, or "host" alone.
func (t Target) Destination() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// connectTimeoutOpt renders d as an ssh `-o` ConnectTimeout value in whole
// seconds (at least 1, since ssh treats 0 as no timeout).
func connectTimeoutOpt(d time.Duration) string {
	return fmt.Sprintf("ConnectTimeout=%d", max(int(d/time.Second), 1))
}

// baseArgs builds the ssh argument prefix (destination and login user).
func (t Target) baseArgs() []string {
	var args []string
	if t.User != "" {
		args = append(args, "-l", t.User)
	}
	args = append(args, t.Host)
	return args
}

// ReachableWithin reports whether an SSH connection to the target currently
// succeeds within connectTimeout.
func ReachableWithin(ctx context.Context, t Target, connectTimeout time.Duration) bool {
	return probe(ctx, t, connectTimeout) == nil
}

// probe attempts an SSH connection to the target within connectTimeout. It runs
// a non-interactive `ssh ... true` so it inherits the user's SSH configuration
// without ever prompting. On failure the error carries ssh's own last stderr
// line (e.g. "Permission denied (publickey)"), which names the actual cause.
func probe(ctx context.Context, t Target, connectTimeout time.Duration) error {
	// Bounded: OpenSSH prints the server's pre-auth banner here, and a hostile
	// or broken endpoint could otherwise stream into this buffer for as long as
	// the probe runs.
	stderr := &tailBuffer{limit: 8 << 10}
	cmd := exec.CommandContext(ctx, "ssh", RemoteArgs(t, connectTimeout, "true")...)
	cmd.Stderr = stderr
	return probeErr(cmd.Run(), stderr.String())
}

// tailBuffer keeps the last limit bytes written to it. The tail, not the head:
// OpenSSH prints the server's banner before its own diagnostic, and probeErr
// wants that last line. Write always reports the full input as consumed — a
// short count would make os/exec fail the command with io.ErrShortWrite.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.buf) }

// probeErr annotates a failed probe with the last non-empty line ssh wrote to
// stderr; a nil runErr stays nil. The line is quoted with %q: it can carry
// server-controlled bytes (OpenSSH prints the pre-auth Banner to stderr), and
// this error is printed to the user's terminal, so control characters must not
// reach it raw.
func probeErr(runErr error, stderr string) error {
	if runErr == nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return runErr
	}
	const maxLine = 200 // bound what a hostile server can make us render
	if r := []rune(last); len(r) > maxLine {
		last = string(r[:maxLine]) + "…"
	}
	return fmt.Errorf("%q (%w)", last, runErr)
}

// RemoteArgs builds the ssh argument list for a non-interactive remote command
// (the reachability probe, doctor's checks). Agent forwarding is disabled: each
// call opens a session, so the VM's sshrc would otherwise relink the stable
// agent socket to this short-lived connection and leave it dangling once the
// command exits. Connection multiplexing is disabled too: a probe must neither
// spawn a ControlPersist master (an open inbound connection the activity emitter
// counts, holding the VM awake) nor ride an existing one.
func RemoteArgs(t Target, connectTimeout time.Duration, cmd string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", connectTimeoutOpt(connectTimeout),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ForwardAgent=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
	}
	args = append(args, t.baseArgs()...)
	return append(args, cmd)
}

// WaitReachable polls until the target is reachable, ctx is cancelled, or the
// timeout elapses. connectTimeout bounds each individual probe. The timeout
// error includes the last probe's ssh error, so the cause is not lost.
func WaitReachable(ctx context.Context, t Target, connectTimeout, waitTimeout, interval time.Duration) error {
	return waitReachable(ctx, t, waitTimeout, interval, func(ctx context.Context) error {
		return probe(ctx, t, connectTimeout)
	})
}

// waitReachable is WaitReachable with the probe injected, for tests.
func waitReachable(ctx context.Context, t Target, waitTimeout, interval time.Duration, try func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	var lastErr error
	for {
		err := try(ctx)
		if err == nil {
			return nil
		}
		// A probe killed by the deadline says nothing about the cause; keep the
		// last one that finished on its own.
		if ctx.Err() == nil {
			lastErr = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("ssh to %q not reachable within %s (last error: %v): %w", t.Host, waitTimeout, lastErr, context.DeadlineExceeded)
			}
			return fmt.Errorf("ssh to %q not reachable within %s: %w", t.Host, waitTimeout, context.DeadlineExceeded)
		case <-timer.C:
		}
	}
}

// Exec replaces the current process with an interactive ssh session. It only
// returns on failure to start.
func Exec(t Target, extra ...string) error {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh client not found on PATH: %w", err)
	}
	argv := append([]string{"ssh"}, t.baseArgs()...)
	argv = append(argv, extra...)
	return syscall.Exec(path, argv, os.Environ())
}
