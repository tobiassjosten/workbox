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
	"github.com/tobiassjosten/workbox/internal/schedule"
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

// Remote commands the activity-emitter check runs; the units are installed by
// infra/cloud-init.sh.tftpl.
const (
	activityTimerActiveCmd   = "systemctl is-active --quiet workbox-activity.timer"
	activityServiceFailedCmd = "systemctl is-failed --quiet workbox-activity.service"
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
	// Activity reads the emitter's last-active guest attribute; nil skips that
	// check. *compute.GCP satisfies it.
	Activity compute.Activity

	// now returns the current time (defaults to time.Now).
	now func() time.Time
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
	if d.now == nil {
		d.now = time.Now
	}
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

	// GCP instance state (read-only). stateKnown distinguishes "GCP said
	// UNKNOWN" from "we never got an answer", so the skips below do not claim a
	// state that was never read.
	state, stateKnown := compute.Unknown, false
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
			state, stateKnown = s, true
			add(Result{Name: "gcp instance", Level: Pass, Detail: "state is " + s.String()})
		}
	}

	// Activity signal: a Cloud API read (no SSH), so it runs before the SSH-gated
	// checks below — its 403 remedy matters most when connectivity is also broken.
	// It proves the guest attribute is written and readable (the startup script
	// writes it at boot, so the "activity emitter" check below covers the emitter
	// itself), and is checked even with idle shutdown disabled since
	// `workbox status` reads the same signal.
	if d.Activity == nil {
		add(Result{Name: "activity signal", Level: Skip, Detail: "no activity client"})
	} else if state != compute.Running {
		add(Result{Name: "activity signal", Level: Skip,
			Detail: instanceStateDetail(state, stateKnown) + "; activity is only reported while RUNNING"})
	} else {
		add(d.activitySignal(ctx))
	}

	// Remote checks only when the VM is already running.
	if state != compute.Running {
		add(Result{Name: "ssh reachability", Level: Skip,
			Detail: instanceStateDetail(state, stateKnown) + "; not waking it (use `workbox wake` first)"})
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

	// Activity emitter: the systemd timer that reports last-active. Checked even
	// with idle shutdown disabled, since `workbox status` reads the same signal.
	if err := d.runRemote(ctx, d.Target, activityTimerActiveCmd); err != nil {
		add(Result{Name: "activity emitter", Level: Warn,
			Detail: "workbox-activity.timer is not active",
			Remedy: "reboot the VM (`sudo reboot` on it, or stop the instance with the Console or `gcloud compute instances stop`, then `workbox wake`; `workbox sleep` only suspends and does not re-run provisioning) or run `sudo google_metadata_script_runner startup` on it, so activity is reported for `workbox status` and idle-suspend"})
	} else if err := d.runRemote(ctx, d.Target, activityServiceFailedCmd); err == nil {
		// is-failed succeeds only when the unit's last run failed.
		add(Result{Name: "activity emitter", Level: Warn,
			Detail: "timer active but the last workbox-activity.service run failed",
			Remedy: "on the workbox run `journalctl -u workbox-activity.service` to see why"})
	} else {
		add(Result{Name: "activity emitter", Level: Pass, Detail: "workbox-activity.timer active"})
	}

	return out
}

// instanceStateDetail names the instance state when it was actually read, and
// says so when it was not; the "gcp instance" check carries the reason.
func instanceStateDetail(state compute.State, known bool) string {
	if !known {
		return "instance state could not be read (see the gcp instance check above)"
	}
	return "instance is " + state.String()
}

func (d *Deps) activitySignal(ctx context.Context) Result {
	const name = "activity signal"
	t, ok, err := d.Activity.LastActive(ctx)
	switch {
	case errors.Is(err, compute.ErrInvalidActivity):
		return Result{Name: name, Level: Warn, Detail: err.Error() + "; ignored by workbox, so it cannot hold off idle shutdown",
			Remedy: "something other than the activity emitter wrote the attribute; the emitter's next report overwrites it"}
	case err != nil:
		remedy := "retry; if it persists, check your ADC credentials (`gcloud auth application-default login`) and the Compute API's availability for the project"
		if compute.IsPermissionDenied(err) {
			remedy = "run `make tf-apply` to update the workboxOperator role (adds compute.instances.getGuestAttributes) and make sure it is granted to you"
		}
		return Result{Name: name, Level: Warn, Detail: err.Error(), Remedy: remedy}
	case !ok:
		// The startup script writes a value at every boot, so a running VM without
		// one has not run the current provisioning, or its report failed.
		detail := "no last-active value (the startup script reports one at every boot, so the current provisioning has likely not run since it was applied, or its report failed)"
		if d.Config != nil && d.Config.Schedule.IdleTimeout() > 0 {
			detail += "; the reconciler counts idle time from the VM's last start"
		}
		return Result{Name: name, Level: Warn,
			Detail: detail,
			Remedy: "reboot the VM (`sudo reboot` on it) or run `sudo google_metadata_script_runner startup` on it; if it is still missing, check `journalctl -u google-startup-scripts -u workbox-activity.service` for \"activity report failed\" and that the instance has enable-guest-attributes=TRUE"}
	case schedule.FutureActivity(t, d.now()):
		return Result{Name: name, Level: Warn,
			Detail: "last active " + d.fmtTime(t) + " is in the future; not counted as activity, so idle shutdown still applies",
			Remedy: "check the VM clock (`timedatectl` on it) and whether anything other than the activity emitter writes the attribute"}
	default:
		return Result{Name: name, Level: Pass, Detail: "last active " + d.fmtTime(t)}
	}
}

// fmtTime renders t in the schedule timezone, as `workbox status` does, so the
// two surfaces name the same wall-clock time.
func (d *Deps) fmtTime(t time.Time) string {
	if d.Config != nil {
		if loc, err := time.LoadLocation(d.Config.Schedule.Timezone); err == nil {
			return t.In(loc).Format(schedule.TimeLayout)
		}
	}
	return t.Format(time.RFC3339)
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
