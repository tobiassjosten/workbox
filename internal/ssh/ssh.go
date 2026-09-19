// Package ssh reaches the workbox over the user's real OpenSSH client. It never
// implements an SSH stack in Go: connectivity checks and interactive sessions
// both run the system `ssh`, so the user inherits ~/.ssh/config, ssh-agent,
// known_hosts, ProxyJump and ControlMaster exactly as usual.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// baseArgs builds the ssh argument prefix (destination and login user).
func (t Target) baseArgs() []string {
	var args []string
	if t.User != "" {
		args = append(args, "-l", t.User)
	}
	args = append(args, t.Host)
	return args
}

// Reachable reports whether an SSH connection to the target currently succeeds.
// It runs a non-interactive `ssh ... true` with a short timeout so it inherits
// the user's SSH configuration without ever prompting.
func Reachable(ctx context.Context, t Target) bool {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=accept-new",
	}
	args = append(args, t.baseArgs()...)
	args = append(args, "true")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	return cmd.Run() == nil
}

// WaitReachable polls until the target is reachable, ctx is cancelled, or the
// timeout elapses.
func WaitReachable(ctx context.Context, t Target, timeout, interval time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if Reachable(ctx, t) {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("ssh to %q not reachable within %s: %w", t.Host, timeout, context.DeadlineExceeded)
			}
			return ctx.Err()
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
