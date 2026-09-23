package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/ssh"
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
