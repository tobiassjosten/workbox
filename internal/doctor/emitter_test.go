package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The activity emitter decides, on the VM, whether workbox is "in use": an
// established inbound SSH connection, or a herdr agent reporting `working`.
// `bash -n` only proves it parses, so run it against stubbed ss/herdr/jq/curl
// and check which situations produce a report.
func TestActivityEmitterReportsWhenActive(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}

	raw, err := os.ReadFile("../../infra/cloud-init.sh.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(raw), "<<'ACTIVITY'\n")
	if !ok {
		t.Fatal("infra/cloud-init.sh.tftpl no longer has the ACTIVITY heredoc")
	}
	body, _, ok = strings.Cut(body, "\nACTIVITY\n")
	if !ok {
		t.Fatal("infra/cloud-init.sh.tftpl ACTIVITY heredoc is unterminated")
	}
	// The template's ${activity_key} renders to the guest-attributes path.
	script := strings.ReplaceAll(body, "${activity_key}", "workbox/last_active")

	for _, tc := range []struct {
		name       string
		sshSockets string // what `ss` prints
		agents     string // what `herdr agent list` prints
		wantReport bool
	}{
		{
			name:       "established ssh connection",
			sshSockets: "ESTAB 0 0 10.0.0.2:22 10.0.0.9:51000",
			agents:     `{"result":{"agents":[]}}`,
			wantReport: true,
		},
		{
			name:       "herdr agent working",
			agents:     `{"result":{"agents":[{"agent_status":"working"}]}}`,
			wantReport: true,
		},
		{
			name:       "idle agent and no connection",
			agents:     `{"result":{"agents":[{"agent_status":"idle"}]}}`,
			wantReport: false,
		},
		{
			name:       "nothing at all",
			agents:     `{"result":{"agents":[]}}`,
			wantReport: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			reportFile := filepath.Join(dir, "reported")
			stub := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Only answer for the port-22 filter: a changed filter then reads as
			// "no connection" and the test notices.
			stub("ss", `
for arg in "$@"; do
  case "$arg" in
    "( sport = :22 )") printf '%s' `+quote(tc.sshSockets)+`; exit 0 ;;
  esac
done
exit 0
`)
			stub("herdr", "printf '%s' "+quote(tc.agents)+"\n")
			// Record the PUT the emitter makes instead of reaching the metadata
			// server, and fail like curl would if the URL is wrong.
			// The script re-prepends $HOME/.local/bin, which is where this
			// project installs herdr; point HOME at the sandbox so only stubs
			// can be found. `timeout` is absent on stock macOS, so stub it too.
			stub("timeout", "shift\nexec \"$@\"\n")
			stub("curl", `
for arg in "$@"; do
  case "$arg" in
    *guest-attributes/workbox/last_active) echo "$arg" > `+quote(reportFile)+`; exit 0 ;;
  esac
done
exit 1
`)
			scriptPath := filepath.Join(dir, "workbox-activity")
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bash, scriptPath)
			cmd.Env = append(os.Environ(),
				"HOME="+dir,
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("emitter failed: %v\n%s", err, out)
			}
			_, statErr := os.Stat(reportFile)
			if reported := statErr == nil; reported != tc.wantReport {
				t.Errorf("reported activity = %v, want %v\nemitter output:\n%s", reported, tc.wantReport, out)
			}
		})
	}
}

// quote renders s as a single-quoted shell word.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
