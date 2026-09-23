package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// base is a valid config with no working-hours block; minimal adds one.
const base = `
name: workbox
gcp:
  project_id: example-project
  region: europe-north2
  zone: europe-north2-a
  machine_type: e2-custom-8-16384
  boot_disk_gb: 30
  data_disk_gb: 200
machine:
  linux_user: developer
  ssh_public_key_file: ~/.ssh/id_ed25519.pub
tailscale:
  hostname: workbox
schedule:
  timezone: Europe/Stockholm
`

const minimal = base + `  working_hours:
    start: "06:00"
    end: "23:00"
`

func TestParseAndDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.GCP.ImageFamily != "ubuntu-2404-lts-amd64" {
		t.Errorf("ImageFamily default = %q", c.GCP.ImageFamily)
	}
	if c.Machine.DataMount != "/work" {
		t.Errorf("DataMount default = %q", c.Machine.DataMount)
	}
	if c.State.Document != "workbox" {
		t.Errorf("State.Document default = %q, want workbox", c.State.Document)
	}
	if c.State.FirestoreDatabase != "(default)" {
		t.Errorf("FirestoreDatabase default = %q", c.State.FirestoreDatabase)
	}
	if c.Tailscale.SSHTarget != "workbox" {
		t.Errorf("SSHTarget default = %q, want workbox", c.Tailscale.SSHTarget)
	}
	if c.GCP.DeletionProtection == nil || !*c.GCP.DeletionProtection {
		t.Errorf("DeletionProtection default should be true")
	}
	// Idle timeout defaults to 30 minutes when the key is absent.
	if got := c.Schedule.IdleTimeout(); got != 30*time.Minute {
		t.Errorf("IdleTimeout default = %v, want 30m", got)
	}
	if !c.Schedule.WorkingHours.Active() {
		t.Error("working hours should be active when present without enabled:false")
	}
	sc, err := c.Schedule.Build()
	if err != nil {
		t.Fatalf("Schedule.Build: %v", err)
	}
	if !sc.Enabled {
		t.Error("built schedule should have working hours enabled")
	}
}

func TestWorkingHoursDaysDeduped(t *testing.T) {
	w := &WorkingHours{Days: []string{"mon", "Monday", "tue"}}
	days, err := w.Weekdays()
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0] != time.Monday || days[1] != time.Tuesday {
		t.Errorf("Weekdays = %v, want [Monday Tuesday]", days)
	}
}

func TestUnknownDayRejectedWhenWindowDisabled(t *testing.T) {
	cfg := minimal + "    enabled: false\n    days: [mon, funday]\n"
	if _, err := Parse([]byte(cfg)); err == nil {
		t.Error("an unknown day should be rejected even with enabled: false")
	}
}

func TestLegacyScheduleKeysExplainMigration(t *testing.T) {
	for _, key := range []string{"wake", "sleep"} {
		cfg := base + "  " + key + ": \"06:00\"\n"
		if _, err := Parse([]byte(cfg)); !errors.Is(err, errLegacySchedule) {
			t.Errorf("Parse with schedule.%s: err = %v, want errLegacySchedule", key, err)
		}
	}
}

func TestWorkingHoursDays(t *testing.T) {
	cfg := minimal + "    days: [mon, tue, wed, thu, fri]\n"
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sc, err := c.Schedule.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Days) != 5 {
		t.Errorf("expected 5 weekdays, got %v", sc.Days)
	}
	// Weekday-only window: Monday inside, Saturday outside.
	loc, _ := time.LoadLocation("Europe/Stockholm")
	mon := time.Date(2026, 6, 15, 12, 0, 0, 0, loc)
	sat := time.Date(2026, 6, 13, 12, 0, 0, 0, loc)
	if !sc.WithinWorkingHours(mon) || sc.WithinWorkingHours(sat) {
		t.Error("weekday-only window should include Monday and exclude Saturday")
	}
}

func TestWorkingHoursBadDayRejected(t *testing.T) {
	cfg := minimal + "    days: [mon, funday]\n"
	if _, err := Parse([]byte(cfg)); err == nil {
		t.Fatal("expected error for unknown day name")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(minimal + "bogus: 1\n")); err == nil {
		t.Error("expected error for unknown top-level field")
	}
}

func TestWorkingHoursDisabled(t *testing.T) {
	// enabled:false keeps the window configured but inactive.
	cfg := minimal + `    enabled: false
`
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Schedule.WorkingHours.Active() {
		t.Error("working hours should be inactive when enabled:false")
	}
	sc, err := c.Schedule.Build()
	if err != nil {
		t.Fatal(err)
	}
	if sc.Enabled {
		t.Error("built schedule should have working hours disabled")
	}
}

func TestNoWorkingHoursBlock(t *testing.T) {
	c, err := Parse([]byte(base + "  idle_timeout_minutes: 0\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Schedule.WorkingHours.Active() {
		t.Error("no working_hours block means inactive")
	}
	if got := c.Schedule.IdleTimeout(); got != 0 {
		t.Errorf("idle_timeout_minutes: 0 should disable idle shutdown, got %v", got)
	}
	sc, err := c.Schedule.Build()
	if err != nil {
		t.Fatal(err)
	}
	if sc.Enabled {
		t.Error("built schedule should be window-less")
	}
}

// A YAML null (`idle_timeout_minutes:` with no value) defaults like an absent
// key; infra/locals.tf relies on the same.
func TestIdleTimeoutNullDefaults(t *testing.T) {
	c, err := Parse([]byte(base + "  idle_timeout_minutes:\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Schedule.IdleTimeout(); got != 30*time.Minute {
		t.Errorf("IdleTimeout = %v, want 30m", got)
	}
}

func TestValidateStartAfterEndIsRejected(t *testing.T) {
	cfg := base + "  working_hours:\n    start: \"23:00\"\n    end: \"06:00\"\n"
	if _, err := Parse([]byte(cfg)); err == nil {
		t.Fatal("expected error when working_hours.start is after end")
	}
}

func TestValidateIdleTimeoutBounds(t *testing.T) {
	for _, tc := range []struct {
		minutes string
		ok      bool
	}{
		{"-5", false},
		{"1", false}, // below the emitter's reporting gap
		{"5", true},  // the minimum
	} {
		_, err := Parse([]byte(base + "  idle_timeout_minutes: " + tc.minutes + "\n"))
		if (err == nil) != tc.ok {
			t.Errorf("idle_timeout_minutes: %s: err = %v, want ok=%v", tc.minutes, err, tc.ok)
		}
	}
}

func TestSSHDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.SSH.ConnectTimeout(); got != 15*time.Second {
		t.Errorf("ConnectTimeout default = %v, want 15s", got)
	}
	if got := c.SSH.WaitTimeout(); got != 180*time.Second {
		t.Errorf("WaitTimeout default = %v, want 180s", got)
	}
}

func TestSSHConfigured(t *testing.T) {
	cfg := minimal + `ssh:
  connect_timeout_seconds: 30
  wait_timeout_seconds: 600
`
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.SSH.ConnectTimeout(); got != 30*time.Second {
		t.Errorf("ConnectTimeout = %v, want 30s", got)
	}
	if got := c.SSH.WaitTimeout(); got != 600*time.Second {
		t.Errorf("WaitTimeout = %v, want 600s", got)
	}
}

func TestValidateBadSSHRejected(t *testing.T) {
	for _, bad := range []string{
		"ssh:\n  connect_timeout_seconds: 0\n",
		"ssh:\n  wait_timeout_seconds: -1\n",
	} {
		if _, err := Parse([]byte(minimal + bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestValidateNegativeSwapRejected(t *testing.T) {
	cfg := strings.Replace(base, "  linux_user: developer\n", "  linux_user: developer\n  swap_gb: -1\n", 1)
	if _, err := Parse([]byte(cfg)); err == nil {
		t.Fatal("expected error for negative machine.swap_gb")
	}
}

func TestResolvePathPrecedence(t *testing.T) {
	t.Setenv("WORKBOX_CONFIG", "/env/path.yaml")
	// Explicit flag wins over env.
	got, err := ResolvePath("/flag/path.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/flag/path.yaml" {
		t.Errorf("flag precedence: got %q", got)
	}
	// Env wins over XDG default.
	got, err = ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/env/path.yaml" {
		t.Errorf("env precedence: got %q", got)
	}
	// XDG default.
	os.Unsetenv("WORKBOX_CONFIG")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err = ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/xdg", "workbox", "config.yaml") {
		t.Errorf("xdg path: got %q", got)
	}
}

func TestExpandUser(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, tc := range []struct{ in, want string }{
		{"~/.ssh/id_ed25519.pub", filepath.Join(home, ".ssh/id_ed25519.pub")},
		{"~", home},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
	} {
		got, err := expandUser(tc.in)
		if err != nil {
			t.Fatalf("expandUser(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("expandUser(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

// The shipped example is what `make configure` installs, so it must always
// parse and validate against the current schema.
func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "workbox.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Errorf("workbox.example.yaml does not parse: %v", err)
	}
}

// readInfra returns a file under infra/, for tests pinning the Terraform side of
// a contract.
func readInfra(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../infra/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The enrollment key's metadata entry must be the one ignore_changes names, or a
// replaced key would reach a running VM (see infra/compute.tf).
func TestTailscaleKeyEntryIsIgnored(t *testing.T) {
	m := regexp.MustCompile(`ts_authkey_attr\s*=\s*"([^"]+)"`).FindStringSubmatch(readInfra(t, "locals.tf"))
	if m == nil {
		t.Fatal("infra/locals.tf no longer defines ts_authkey_attr")
	}
	want := regexp.MustCompile(`ignore_changes\s*=\s*\[metadata\["` + regexp.QuoteMeta(m[1]) + `"\]\]`)
	if !want.MatchString(readInfra(t, "compute.tf")) {
		t.Errorf("infra/compute.tf does not ignore_changes metadata[%q]", m[1])
	}
}

// TestTerraformMirrorsDefaults checks the values Terraform duplicates from this
// package (infra/locals.tf, infra/workflow.tf), so a one-sided change fails.
func TestTerraformMirrorsDefaults(t *testing.T) {
	locals, workflow := readInfra(t, "locals.tf"), readInfra(t, "workflow.tf")
	if want := fmt.Sprintf("idle_timeout_minutes, null), %d)", defaultIdleTimeoutMinutes); !strings.Contains(locals, want) {
		t.Errorf("infra/locals.tf idle default: missing %s", want)
	}
	if want := fmt.Sprintf("local.idle_min >= %d", minIdleTimeoutMinutes); !strings.Contains(workflow, want) {
		t.Errorf("infra/workflow.tf idle minimum: missing %s", want)
	}
	// config.Weekdays dedupes repeated spellings, so Terraform must too or
	// [mon, monday] fails every plan with a duplicate-key error.
	if want := "distinct(local.work_days)"; !strings.Contains(locals, want) {
		t.Errorf("infra/locals.tf work_days_map: missing %s", want)
	}
	// The CLI and the reconciler must resolve the same Firestore document; a
	// one-sided default change would silently split their state.
	for _, want := range []string{
		fmt.Sprintf(`firestore_database, ""), %q)`, defaultFirestoreDatabase),
		fmt.Sprintf(`state.collection, ""), %q)`, defaultStateCollection),
		`try(local.cfg.state.document, ""), local.name)`,
	} {
		if !strings.Contains(locals, want) {
			t.Errorf("infra/locals.tf state defaults: missing %s", want)
		}
	}
	// The retired-key precondition must stay on the Terraform side too, or an
	// unmigrated config would apply an idle-only reconciler.
	if want := "try(local.cfg.schedule.wake, null) == null"; !strings.Contains(workflow, want) {
		t.Errorf("infra/workflow.tf retired-key precondition: missing %s", want)
	}
	// minIdleTimeoutMinutes assumes the emitter reports about once a minute; a
	// slower timer would make the floor unsafe.
	cloudInit := readInfra(t, "cloud-init.sh.tftpl")
	for _, want := range []string{"OnUnitActiveSec=1min", "AccuracySec=15s"} {
		if !strings.Contains(cloudInit, want) {
			t.Errorf("infra/cloud-init.sh.tftpl emitter timer: missing %s (minIdleTimeoutMinutes assumes it)", want)
		}
	}
	// docs/security.md states both of these as the bounds on how long a socket
	// can count as activity: an unauthenticated connection (LoginGraceTime) and
	// a dead session (ClientAlive*).
	for _, want := range []string{"LoginGraceTime 30", "ClientAliveInterval 60", "ClientAliveCountMax 3"} {
		if !strings.Contains(cloudInit, want) {
			t.Errorf("infra/cloud-init.sh.tftpl sshd: missing %s (docs/security.md relies on it)", want)
		}
	}
	for name, wd := range weekdayNames {
		if want := fmt.Sprintf("%s = %d", name, int(wd)); !strings.Contains(locals, want) {
			t.Errorf("infra/locals.tf work_day_num: missing %s", want)
		}
	}
}
