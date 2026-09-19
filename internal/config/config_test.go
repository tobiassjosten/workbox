package config

import (
	"os"
	"path/filepath"
	"testing"
)

const minimal = `
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
  wake: "06:00"
  sleep: "23:00"
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
	if _, err := c.Schedule.Build(); err != nil {
		t.Errorf("Schedule.Build: %v", err)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(minimal + "bogus: 1\n")); err == nil {
		t.Error("expected error for unknown top-level field")
	}
}

func TestValidateWakeAfterSleepIsRejected(t *testing.T) {
	_, err := Parse([]byte(`
name: workbox
gcp:
  project_id: p
  region: r
  zone: z
  machine_type: m
  boot_disk_gb: 30
  data_disk_gb: 200
machine:
  linux_user: developer
  ssh_public_key_file: ~/.ssh/id_ed25519.pub
tailscale:
  hostname: workbox
schedule:
  timezone: Europe/Stockholm
  wake: "23:00"
  sleep: "06:00"
`))
	if err == nil {
		t.Fatal("expected error when wake is after sleep")
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
