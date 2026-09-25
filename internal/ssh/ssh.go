// Package ssh reaches the workbox over the user's real OpenSSH client. It never
// implements an SSH stack in Go: connectivity checks and interactive sessions
// both run the system `ssh`, so the user inherits ~/.ssh/config, ssh-agent,
// known_hosts, ProxyJump and ControlMaster exactly as usual (the non-interactive
// probes opt out of multiplexing entirely, and `forward` never becomes the
// master; see RemoteArgs and ExecForward).
package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
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
func waitReachable(
	ctx context.Context,
	t Target,
	waitTimeout, interval time.Duration,
	try func(context.Context) error,
) error {
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
				return fmt.Errorf("ssh to %q not reachable within %s (last error: %w): %w",
					t.Host, waitTimeout, lastErr, context.DeadlineExceeded)
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

// Forward is a local→VM port forward: a local port tunneled to the same service
// listening on the VM's loopback. Because the VM side is always localhost, the
// forwarded service is only ever reachable through this tunnel — nothing is
// exposed on the tailnet or anywhere else.
type Forward struct {
	Local  int
	Remote int
}

// Arg renders the forward as the value for ssh's -L option. The local end is
// bound explicitly to localhost so the tunnel is never published on other
// interfaces, whatever the user's GatewayPorts setting says.
func (f Forward) Arg() string {
	return fmt.Sprintf("localhost:%d:localhost:%d", f.Local, f.Remote)
}

// ParseForwards parses several port specs, rejecting a local port used twice:
// ssh would only fail on that after connecting (ExitOnForwardFailure), which for
// workbox means after the wake and the wait for SSH.
func ParseForwards(specs []string) ([]Forward, error) {
	forwards := make([]Forward, 0, len(specs))
	seen := make(map[int]bool, len(specs))
	for _, s := range specs {
		f, err := parseForward(s)
		if err != nil {
			return nil, err
		}
		if seen[f.Local] {
			return nil, fmt.Errorf("local port %d is forwarded twice", f.Local)
		}
		seen[f.Local] = true
		forwards = append(forwards, f)
	}
	return forwards, nil
}

// CheckLocalPorts reports the first local forward port that cannot be bound
// (typically already in use), so `workbox forward` can fail before waking the
// VM rather than after, when ssh's ExitOnForwardFailure would trip. ssh's -L
// binds every address "localhost" resolves to, so both loopback families are
// checked: binding the name alone would pick one and miss a port held on the
// other.
func CheckLocalPorts(forwards []Forward) error {
	for _, f := range forwards {
		for _, host := range []string{"127.0.0.1", "::1"} {
			ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(f.Local)))
			if err != nil {
				// A host without this family cannot serve the forward on it
				// either, so skip it rather than reporting the port busy.
				if unsupportedFamily(err) {
					continue
				}
				return fmt.Errorf("cannot bind local port %d: %w", f.Local, err)
			}
			_ = ln.Close() // released immediately; ssh binds it again
		}
	}
	return nil
}

// unsupportedFamily reports whether err means the address family is unavailable
// on this host (e.g. IPv6 disabled), as opposed to the port being taken.
func unsupportedFamily(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

// parseForward parses a friendly port spec into a Forward. It accepts "PORT"
// (same port on both ends, the common case) or "LOCAL:REMOTE" to map a local
// port to a different port on the VM.
func parseForward(spec string) (Forward, error) {
	fields := strings.Split(spec, ":")
	var local, remote string
	switch len(fields) {
	case 1:
		local, remote = fields[0], fields[0]
	case 2:
		local, remote = fields[0], fields[1]
	default:
		return Forward{}, fmt.Errorf("invalid port forward %q: expected \"PORT\" or \"LOCAL:REMOTE\"", spec)
	}
	lp, err := parsePort(local)
	if err != nil {
		return Forward{}, fmt.Errorf("invalid port forward %q: %w", spec, err)
	}
	rp, err := parsePort(remote)
	if err != nil {
		return Forward{}, fmt.Errorf("invalid port forward %q: %w", spec, err)
	}
	return Forward{Local: lp, Remote: rp}, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d out of range 1-65535", n)
	}
	return n, nil
}

// ExecForward replaces the current process with a non-interactive ssh session
// that holds the given local port forwards open (ssh -N, no remote shell) until
// interrupted. The argument list, and the reasoning behind its options, is in
// forwardArgv. It only returns on failure to start.
func ExecForward(t Target, forwards []Forward) error {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh client not found on PATH: %w", err)
	}
	return syscall.Exec(path, forwardArgv(t, forwards), os.Environ())
}

// forwardArgv builds the full argv (including argv[0]) for ExecForward. Options
// precede the destination so this works regardless of whether the local OpenSSH
// permutes arguments. ExitOnForwardFailure makes ssh fail loudly if a local port
// is already in use rather than connect silently without the tunnel.
// ForwardAgent=no keeps the tunnel from ever claiming the VM's forwarded-agent
// link. ControlMaster=no keeps the tunnel from ever becoming the multiplexing
// master: as master it would own the user's other sessions (Ctrl-C here would
// drop them) and could leave a master connection behind that the activity
// emitter counts, holding the VM awake. Riding an existing master is fine; that
// one is already counted.
func forwardArgv(t Target, forwards []Forward) []string {
	argv := []string{"ssh", "-N", "-o", "ExitOnForwardFailure=yes", "-o", "ForwardAgent=no", "-o", "ControlMaster=no"}
	for _, f := range forwards {
		argv = append(argv, "-L", f.Arg())
	}
	return append(argv, t.baseArgs()...)
}
