package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/ssh"
	"google.golang.org/api/googleapi"
)

// The units doctor checks are the ones the startup script installs.
func TestActivityUnitsMatchCloudInit(t *testing.T) {
	raw, err := os.ReadFile("../../infra/cloud-init.sh.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{activityTimerActiveCmd, activityServiceFailedCmd} {
		fields := strings.Fields(cmd)
		unit := fields[len(fields)-1]
		if want := "/etc/systemd/system/" + unit; !strings.Contains(string(raw), want) {
			t.Errorf("doctor checks %s, but infra/cloud-init.sh.tftpl does not write %s", unit, want)
		}
	}
}

func find(results []Result, name string) (Result, bool) {
	for _, r := range results {
		if r.Name == name {
			return r, true
		}
	}
	return Result{}, false
}

func TestRunSkipsRemoteWhenSuspended(t *testing.T) {
	d := Deps{
		Config:  &config.Config{},
		Compute: compute.NewFake(compute.Suspended),
		Target:  ssh.Target{Host: "workbox"},
		lookPath: func(string) (string, error) {
			return "/usr/bin/x", nil
		},
	}
	results := Run(context.Background(), d)
	r, ok := find(results, "ssh reachability")
	want := "instance is SUSPENDED; not waking it (use `workbox wake` first)"
	if !ok || r.Level != Skip || r.Detail != want {
		t.Fatalf("ssh reachability = %+v, want SKIP %q", r, want)
	}
	if Failed(results) {
		t.Error("no hard failures expected when suspended with all local bins present")
	}
}

func TestRunReportsRemoteChecksWhenRunning(t *testing.T) {
	remoteCalls := map[string]bool{}
	d := Deps{
		Config:    &config.Config{},
		Compute:   compute.NewFake(compute.Running),
		Target:    ssh.Target{Host: "workbox", User: "developer"},
		lookPath:  func(string) (string, error) { return "/usr/bin/x", nil },
		reachable: func(context.Context, ssh.Target) bool { return true },
		runRemote: func(_ context.Context, _ ssh.Target, cmd string) error {
			remoteCalls[cmd] = true
			switch cmd {
			case "PATH=$HOME/.local/bin:$PATH command -v claude":
				return errors.New("not found") // pretend claude is missing
			case activityServiceFailedCmd:
				return errors.New("not failed") // the emitter's last run succeeded
			}
			return nil
		},
	}
	results := Run(context.Background(), d)
	if r, _ := find(results, "remote herdr"); r.Level != Pass {
		t.Errorf("remote herdr = %+v, want PASS", r)
	}
	if r, _ := find(results, "remote claude"); r.Level != Fail {
		t.Errorf("remote claude = %+v, want FAIL", r)
	}
	if !Failed(results) {
		t.Error("expected Failed to be true when claude missing")
	}
	if r, _ := find(results, "activity emitter"); r.Level != Pass {
		t.Errorf("activity emitter = %+v, want PASS", r)
	}
}

func TestRunWarnsWhenActivityEmitterInactive(t *testing.T) {
	d := Deps{
		Config:    &config.Config{},
		Compute:   compute.NewFake(compute.Running),
		Target:    ssh.Target{Host: "workbox", User: "developer"},
		lookPath:  func(string) (string, error) { return "/usr/bin/x", nil },
		reachable: func(context.Context, ssh.Target) bool { return true },
		runRemote: func(_ context.Context, _ ssh.Target, cmd string) error {
			if cmd == activityTimerActiveCmd {
				return errors.New("inactive")
			}
			return nil
		},
	}
	results := Run(context.Background(), d)
	r, _ := find(results, "activity emitter")
	if r.Level != Warn {
		t.Errorf("activity emitter = %+v, want WARN", r)
	}
	// The remedy must lead to a real boot: `workbox sleep` only suspends.
	if !strings.Contains(r.Remedy, "gcloud compute instances stop") {
		t.Errorf("activity emitter remedy = %q, want it to name a real stop", r.Remedy)
	}
}

func TestRunWarnsWhenActivityServiceFailed(t *testing.T) {
	d := Deps{
		Config:    &config.Config{},
		Compute:   compute.NewFake(compute.Running),
		Target:    ssh.Target{Host: "workbox", User: "developer"},
		lookPath:  func(string) (string, error) { return "/usr/bin/x", nil },
		reachable: func(context.Context, ssh.Target) bool { return true },
		// Everything succeeds, including is-failed: the last run failed.
		runRemote: func(context.Context, ssh.Target, string) error { return nil },
	}
	results := Run(context.Background(), d)
	if r, _ := find(results, "activity emitter"); r.Level != Warn {
		t.Errorf("activity emitter = %+v, want WARN", r)
	}
}

func TestConnectTimeout(t *testing.T) {
	thirty := 30
	cfg := &config.Config{}
	cfg.SSH.ConnectTimeoutSeconds = &thirty
	for _, tc := range []struct {
		name string
		deps Deps
		want time.Duration
	}{
		{"configured", Deps{Config: cfg}, 30 * time.Second},
		{"unset", Deps{Config: &config.Config{}}, 15 * time.Second},
		{"no config", Deps{}, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.deps.connectTimeout(); got != tc.want {
				t.Errorf("connectTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActivitySignal(t *testing.T) {
	forbidden := fmt.Errorf("reading guest attribute: %w", &googleapi.Error{Code: http.StatusForbidden})
	for _, tc := range []struct {
		name       string
		act        compute.FakeActivity
		want       Level
		wantRemedy string
	}{
		{"reported", compute.FakeActivity{ActiveAt: time.Unix(1789930541, 0), ActiveOK: true}, Pass, ""},
		{"never reported", compute.FakeActivity{}, Warn, "google_metadata_script_runner"},
		{"far future", compute.FakeActivity{ActiveAt: time.Unix(1789930541, 0).Add(10 * time.Minute), ActiveOK: true}, Warn, "VM clock"},
		{"not a timestamp", compute.FakeActivity{ActiveErr: fmt.Errorf("guest attribute: %w", compute.ErrInvalidActivity)}, Warn, "something other than the activity emitter"},
		{"forbidden", compute.FakeActivity{ActiveErr: forbidden}, Warn, "workboxOperator"},
		{"other error", compute.FakeActivity{ActiveErr: errors.New("parsing guest attribute")}, Warn, "retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, found := find(Run(context.Background(), activityDeps(tc.act, nil)), "activity signal")
			if !found || r.Level != tc.want {
				t.Fatalf("activity signal = %+v found=%v, want %s", r, found, tc.want)
			}
			if tc.wantRemedy == "" {
				if r.Remedy != "" {
					t.Errorf("a passing check should carry no remedy, got %q", r.Remedy)
				}
				// The timestamp is rendered in the schedule timezone, as
				// `workbox status` renders it.
				loc, err := time.LoadLocation("Europe/Stockholm")
				if err != nil {
					t.Fatal(err)
				}
				want := "last active " + time.Unix(1789930541, 0).In(loc).Format(schedule.TimeLayout)
				if r.Detail != want {
					t.Errorf("detail = %q, want %q", r.Detail, want)
				}
			} else if !strings.Contains(r.Remedy, tc.wantRemedy) {
				t.Errorf("remedy = %q, want it to contain %q", r.Remedy, tc.wantRemedy)
			}
		})
	}
}

// `workbox status` reads the same signal whatever the idle setting, so doctor
// keeps diagnosing it when idle shutdown is disabled — without the idle note.
func TestActivitySignalCheckedWhenIdleDisabled(t *testing.T) {
	const idleNote = "the reconciler counts idle time"
	zero := 0
	r, found := find(Run(context.Background(), activityDeps(compute.FakeActivity{}, &zero)), "activity signal")
	if !found {
		t.Fatal("activity signal should be checked even when idle shutdown is disabled")
	}
	if strings.Contains(r.Detail, idleNote) {
		t.Errorf("idle disabled: detail %q should not mention idle counting", r.Detail)
	}
	r, _ = find(Run(context.Background(), activityDeps(compute.FakeActivity{}, nil)), "activity signal")
	if !strings.Contains(r.Detail, idleNote) {
		t.Errorf("idle enabled: detail %q should mention idle counting", r.Detail)
	}
}

// The activity signal is a Cloud API read, so it is reported even when SSH is
// broken — that is when its IAM remedy matters most.
func TestActivitySignalReportedWhenUnreachable(t *testing.T) {
	d := activityDeps(compute.FakeActivity{ActiveAt: time.Unix(1789930541, 0), ActiveOK: true}, nil)
	d.reachable = func(context.Context, ssh.Target) bool { return false }
	results := Run(context.Background(), d)
	if r, found := find(results, "activity signal"); !found || r.Level != Pass {
		t.Errorf("activity signal = %+v found=%v, want PASS even when ssh fails", r, found)
	}
	if r, _ := find(results, "ssh reachability"); r.Level != Fail {
		t.Errorf("ssh reachability = %+v, want FAIL", r)
	}
}

// Without a Compute client there is nothing to read the signal with.
func TestActivitySignalSkippedWithoutClient(t *testing.T) {
	d := activityDeps(compute.FakeActivity{}, nil)
	d.Activity = nil
	r, found := find(Run(context.Background(), d), "activity signal")
	if !found || r.Level != Skip || r.Detail != "no activity client" {
		t.Errorf("activity signal = %+v found=%v, want SKIP \"no activity client\"", r, found)
	}
}

// A suspended VM reports no activity by design; doctor must not warn about it.
func TestActivitySignalSkippedWhenNotRunning(t *testing.T) {
	d := activityDeps(compute.FakeActivity{}, nil)
	d.Compute = compute.NewFake(compute.Suspended)
	r, found := find(Run(context.Background(), d), "activity signal")
	want := "instance is SUSPENDED; activity is only reported while RUNNING"
	if !found || r.Level != Skip || r.Detail != want {
		t.Errorf("activity signal = %+v found=%v, want SKIP %q", r, found, want)
	}
}

// activityDeps builds Deps for a running, reachable VM whose emitter is healthy.
func activityDeps(act compute.FakeActivity, idleMinutes *int) Deps {
	cfg := &config.Config{}
	cfg.Schedule.IdleTimeoutMinutes = idleMinutes
	cfg.Schedule.Timezone = "Europe/Stockholm"
	return Deps{
		Config:    cfg,
		Compute:   compute.NewFake(compute.Running),
		Activity:  act,
		Target:    ssh.Target{Host: "workbox", User: "developer"},
		now:       func() time.Time { return time.Unix(1789930541, 0) },
		lookPath:  func(string) (string, error) { return "/usr/bin/x", nil },
		reachable: func(context.Context, ssh.Target) bool { return true },
		runRemote: func(_ context.Context, _ ssh.Target, cmd string) error {
			if cmd == activityServiceFailedCmd {
				return errors.New("not failed")
			}
			return nil
		},
	}
}

func TestRunFailsOnComputeError(t *testing.T) {
	d := Deps{
		Config:   &config.Config{},
		Compute:  erroringCompute{},
		Target:   ssh.Target{Host: "workbox"},
		lookPath: func(string) (string, error) { return "/usr/bin/x", nil },
	}
	results := Run(context.Background(), d)
	if r, _ := find(results, "gcp instance"); r.Level != Fail {
		t.Errorf("gcp instance = %+v, want FAIL", r)
	}
	// The state was never read, so the skips must not name one.
	want := "instance state could not be read (see the gcp instance check above); " +
		"not waking it (use `workbox wake` first)"
	if r, _ := find(results, "ssh reachability"); r.Level != Skip || r.Detail != want {
		t.Errorf("ssh reachability = %+v, want SKIP %q", r, want)
	}
}

type erroringCompute struct{}

func (erroringCompute) Status(context.Context) (compute.State, error) {
	return compute.Unknown, errors.New("cannot read instance")
}
func (erroringCompute) Start(context.Context) error   { return nil }
func (erroringCompute) Resume(context.Context) error  { return nil }
func (erroringCompute) Suspend(context.Context) error { return nil }
