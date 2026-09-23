// Package doctor runs read-only diagnostics against the local environment and
// the remote workbox. It never mutates cloud state. By default it never wakes
// a sleeping VM; remote checks are skipped unless the instance is already
// RUNNING. The caller (cmd/workbox) may wake the VM first via --wake.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/herdr"
	"github.com/tobiassjosten/workbox/internal/ssh"
)

// Level is the outcome of a check.
type Level string

const (
	// Pass means the check succeeded.
	Pass Level = "PASS"
	// Warn means a non-fatal issue.
	Warn Level = "WARN"
	// Fail means a problem that blocks normal operation.
	Fail Level = "FAIL"
	// Skip means the check could not run in the current state.
	Skip Level = "SKIP"
)

// Result is the outcome of a single check.
type Result struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// Deps are the collaborators the diagnostics use. Fields left nil cause the
// dependent checks to be skipped. The function hooks default to real
// implementations in Run and are overridable in tests.
type Deps struct {
	Config     *config.Config
	Compute    compute.Compute
	ComputeErr error // if non-nil, the gcp instance check reports FAIL with this error
	Target     ssh.Target

	// lookPath reports whether a local binary exists (defaults to exec.LookPath).
	lookPath func(string) (string, error)
	// runRemote runs a command on the workbox over ssh and returns success
	// (defaults to a BatchMode ssh exec).
	runRemote func(ctx context.Context, t ssh.Target, cmd string) error
	// reachable reports SSH reachability (defaults to ssh.ReachableWithin with
	// the configured connect timeout).
	reachable func(ctx context.Context, t ssh.Target) bool
}

func (d *Deps) defaults() {
	if d.lookPath == nil {
		d.lookPath = exec.LookPath
	}
	timeout := d.connectTimeout()
	if d.reachable == nil {
		d.reachable = func(ctx context.Context, t ssh.Target) bool {
			return ssh.ReachableWithin(ctx, t, timeout)
		}
	}
	if d.runRemote == nil {
		d.runRemote = func(ctx context.Context, t ssh.Target, cmd string) error {
			return exec.CommandContext(ctx, "ssh", ssh.RemoteArgs(t, timeout, cmd)...).Run()
		}
	}
}

// connectTimeout is the SSH connect timeout every doctor probe uses, so a
// loaded VM that passes reachability doesn't then fail the remote checks. A nil
// Config (the "config not loaded" path) yields the default.
func (d *Deps) connectTimeout() time.Duration {
	var cfgSSH config.SSH // zero value yields the default timeout
	if d.Config != nil {
		cfgSSH = d.Config.SSH
	}
	return cfgSSH.ConnectTimeout()
}

// Run executes all diagnostics in order and returns their results.
func Run(ctx context.Context, d Deps) []Result {
	d.defaults()
	var out []Result
	add := func(r Result) { out = append(out, r) }

	// Config.
	if d.Config == nil {
		add(Result{Name: "config", Level: Fail, Detail: "config not loaded",
			Remedy: "run `make configure` to regenerate the config file"})
	} else {
		add(Result{Name: "config", Level: Pass, Detail: "loaded"})
	}

	// Local binaries.
	add(d.localBinary("tailscale CLI", "tailscale",
		"install Tailscale from https://tailscale.com/download"))
	add(d.localBinary("local herdr", herdr.Binary,
		"install Herdr from https://herdr.dev/docs/install/"))
	add(d.localBinary("ssh client", "ssh", "install OpenSSH"))

	// GCP instance state (read-only).
	state := compute.Unknown
	if d.ComputeErr != nil {
		add(Result{Name: "gcp instance", Level: Fail, Detail: d.ComputeErr.Error(),
			Remedy: "check ADC: gcloud auth application-default login"})
	} else if d.Compute == nil {
		add(Result{Name: "gcp instance", Level: Skip, Detail: "no compute client"})
	} else {
		s, err := d.Compute.Status(ctx)
		if err != nil {
			add(Result{Name: "gcp instance", Level: Fail, Detail: err.Error(),
				Remedy: "check ADC (`gcloud auth application-default login`) and gcp.project_id/zone/name"})
		} else {
			state = s
			add(Result{Name: "gcp instance", Level: Pass, Detail: "state is " + s.String()})
		}
	}

	// Remote checks only when the VM is already running.
	if state != compute.Running {
		add(Result{Name: "ssh reachability", Level: Skip,
			Detail: "instance is not RUNNING; not waking it (use `workbox wake` first)"})
		return out
	}

	if !d.reachable(ctx, d.Target) {
		add(Result{Name: "ssh reachability", Level: Fail,
			Detail: fmt.Sprintf("cannot ssh to %q", d.Target.Host),
			Remedy: "check Tailscale is up and the SSH host/alias resolves; see docs/operations.md"})
		return out
	}
	add(Result{Name: "ssh reachability", Level: Pass, Detail: "ssh to " + d.Target.Host + " works"})

	// "herdr" is the fixed VM binary name; the locally-overridable herdr.Binary is only for the local check.
	add(d.remoteBinary(ctx, "remote herdr", "herdr",
		"re-provision the VM or install Herdr on it"))
	add(d.remoteBinary(ctx, "remote claude", "claude",
		"re-provision the VM or run the Claude install on it"))

	// Herdr/Claude integration hook.
	if err := d.runRemote(ctx, d.Target, "test -f ~/.claude/hooks/herdr-agent-state.sh"); err != nil {
		add(Result{Name: "herdr/claude integration", Level: Warn,
			Detail: "integration hook not found",
			Remedy: "on the workbox run `herdr integration install claude`"})
	} else {
		add(Result{Name: "herdr/claude integration", Level: Pass, Detail: "hook installed"})
	}

	return out
}

func (d *Deps) localBinary(name, bin, remedy string) Result {
	if _, err := d.lookPath(bin); err != nil {
		return Result{Name: name, Level: Fail, Detail: bin + " not found on PATH", Remedy: remedy}
	}
	return Result{Name: name, Level: Pass, Detail: bin + " found"}
}

func (d *Deps) remoteBinary(ctx context.Context, name, bin, remedy string) Result {
	if err := d.runRemote(ctx, d.Target, "PATH=$HOME/.local/bin:$PATH command -v "+bin); err != nil {
		return Result{Name: name, Level: Fail, Detail: bin + " not found on the workbox", Remedy: remedy}
	}
	return Result{Name: name, Level: Pass, Detail: bin + " found"}
}

// Failed reports whether any result is a hard failure.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

// ErrChecksFailed is returned by the command when a check fails.
var ErrChecksFailed = errors.New("one or more diagnostics failed")
