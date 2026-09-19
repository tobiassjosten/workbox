package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/ssh"
)

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
	if !ok || r.Level != Skip {
		t.Fatalf("ssh reachability = %+v, want SKIP", r)
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
			// Pretend claude is missing.
			if cmd == "PATH=$HOME/.local/bin:$PATH command -v claude" {
				return errors.New("not found")
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
}

type erroringCompute struct{}

func (erroringCompute) Status(context.Context) (compute.State, error) {
	return compute.Unknown, errors.New("cannot read instance")
}
func (erroringCompute) Start(context.Context) error   { return nil }
func (erroringCompute) Resume(context.Context) error  { return nil }
func (erroringCompute) Suspend(context.Context) error { return nil }
