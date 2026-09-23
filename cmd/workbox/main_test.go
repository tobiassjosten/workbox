package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/cli"
	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"github.com/tobiassjosten/workbox/internal/ssh"
	"github.com/tobiassjosten/workbox/internal/state"
)

func TestDiagnoseUnreachable(t *testing.T) {
	timeout := fmt.Errorf("ssh to %q not reachable: %w", "workbox", context.DeadlineExceeded)
	target := ssh.Target{Host: "workbox", User: "developer"}
	for _, tc := range []struct {
		name     string
		vm       compute.State
		waitErr  error
		wantHint bool
	}{
		{"timeout while running", compute.Running, timeout, true},
		{"timeout while suspended", compute.Suspended, timeout, false},
		{"cancelled", compute.Running, context.Canceled, false},
		// An unreadable instance state still gets the hint: that is exactly when
		// the user needs the troubleshooting list.
		{"status unreadable", compute.Unknown, timeout, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			comp := compute.NewFake(tc.vm)
			if tc.vm == compute.Unknown {
				// A % in the error must not be read as a format verb.
				comp.StatusErr = errors.New("read failed: 50% quota")
			}
			err := diagnoseUnreachable(context.Background(), &buf, comp, target, 3*time.Minute, tc.waitErr)
			if !errors.Is(err, tc.waitErr) {
				t.Errorf("err = %v, want the wait error passed through", err)
			}
			out := buf.String()
			if !tc.wantHint {
				if out != "" {
					t.Errorf("unexpected hint:\n%s", out)
				}
				return
			}
			lead := "The VM reports RUNNING but SSH did not become reachable within 3m0s."
			if tc.vm == compute.Unknown {
				lead = "SSH did not become reachable within 3m0s, and the VM's state could not be read (read failed: 50% quota)."
			}
			for _, want := range []string{
				lead,
				"ssh -v -o ConnectTimeout=60 -o ServerAliveInterval=15 developer@workbox",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("hint missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// Each retired command explains its replacement instead of failing with a bare
// "unknown command" (or, for sleep-at, a suggestion that suspends immediately).
func TestRetiredCommandsExplainReplacement(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"wake-at", "08:00"}, "wake-at was removed: waking is manual now"},
		{[]string{"sleep-at", "20:00"}, "sleep-at was replaced by `workbox sleep HH:MM`"},
		{[]string{"cancel-override"}, "cancel-override was renamed to `workbox cancel`"},
	} {
		t.Run(tc.args[0], func(t *testing.T) {
			root := newRootCmd()
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v error = %v, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}

// testSleepApp builds an App on fakes with a fixed clock.
func testSleepApp(t *testing.T, vm compute.State, now time.Time) (*cli.App, *state.Fake) {
	t.Helper()
	sc, err := schedule.New("06:00", "23:00", "Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Name: "workbox", Schedule: config.Schedule{
		Timezone:     "Europe/Stockholm",
		WorkingHours: &config.WorkingHours{Start: "06:00", End: "23:00"},
	}}
	store := state.NewFake()
	return &cli.App{
		Cfg: cfg, Sched: sc, Compute: compute.NewFake(vm), Store: store,
		Now: func() time.Time { return now }, Out: io.Discard,
	}, store
}

// `sleep` suspends now; `sleep HH:MM` only schedules. Flipping the two would
// suspend the VM the moment a user scheduled a sleep for later.
func TestRunSleepDispatch(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 15, 15, 0, 0, 0, loc)

	app, store := testSleepApp(t, compute.Running, now)
	if err := runSleep(context.Background(), app, []string{"20:00"}); err != nil {
		t.Fatal(err)
	}
	if doc, _ := store.Load(context.Background()); doc.Sleep == nil {
		t.Error("sleep HH:MM should store a scheduled sleep")
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Running {
		t.Errorf("sleep HH:MM changed VM state to %v, want Running", s)
	}

	app, _ = testSleepApp(t, compute.Running, now)
	if err := runSleep(context.Background(), app, nil); err != nil {
		t.Fatal(err)
	}
	if s, _ := app.Compute.Status(context.Background()); s != compute.Suspended {
		t.Errorf("bare sleep left the VM %v, want Suspended", s)
	}
}

func TestSleepRejectsTwoArgs(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"sleep", "20:00", "21:00"})
	if err := root.Execute(); err == nil {
		t.Error("sleep with two arguments should fail")
	}
}

// `forward` rejects a busy local port before waking (and billing) the VM, so it
// returns before any cloud call.
func TestRunForwardChecksPortsBeforeWaking(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(keyPath, []byte("ssh-ed25519 AAAA test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`name: workbox
gcp:
  project_id: p
  region: r
  zone: z
  machine_type: m
  boot_disk_gb: 30
  data_disk_gb: 200
machine:
  linux_user: developer
  ssh_public_key_file: %s
tailscale:
  hostname: workbox
schedule:
  timezone: Europe/Stockholm
`, keyPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	busy := ln.Addr().(*net.TCPAddr).Port

	err = runForward(context.Background(), cfgPath, []string{strconv.Itoa(busy)})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("cannot bind local port %d", busy)) {
		t.Fatalf("runForward = %v, want a cannot-bind error", err)
	}
}
