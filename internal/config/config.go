// Package config loads the canonical workbox configuration shared by the CLI
// and (via yamldecode) by Terraform.
//
// Resolution precedence for the config path:
//
//  1. An explicit path (the --config flag), when non-empty.
//  2. The WORKBOX_CONFIG environment variable.
//  3. $XDG_CONFIG_HOME/workbox/config.yaml, or ~/.config/workbox/config.yaml.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tobiassjosten/workbox/internal/schedule"
	"gopkg.in/yaml.v3"
)

// Config is the full workbox configuration. Every field maps to a documented
// key in workbox.example.yaml.
type Config struct {
	// Name is the logical name of the workbox, used as the Compute Engine
	// instance name and the Firestore state document id.
	Name string `yaml:"name"`

	GCP       GCP       `yaml:"gcp"`
	Machine   Machine   `yaml:"machine"`
	Tailscale Tailscale `yaml:"tailscale"`
	Schedule  Schedule  `yaml:"schedule"`
	State     State     `yaml:"state"`
}

// GCP holds Google Cloud placement and sizing.
type GCP struct {
	ProjectID string `yaml:"project_id"`
	Region    string `yaml:"region"`
	Zone      string `yaml:"zone"`
	// ServicesRegion is consumed only by Terraform (infra/locals.tf), which
	// defaults it to gcp.region. The Go CLI does not read this field.
	ServicesRegion string `yaml:"services_region"`
	MachineType    string `yaml:"machine_type"`

	BootDiskGB int `yaml:"boot_disk_gb"`
	DataDiskGB int `yaml:"data_disk_gb"`

	// Optional. Defaults applied by Defaults().
	ImageFamily        string `yaml:"image_family"`
	ImageProject       string `yaml:"image_project"`
	DeletionProtection *bool  `yaml:"deletion_protection"`
}

// Machine holds guest-OS configuration.
type Machine struct {
	LinuxUser        string `yaml:"linux_user"`
	SSHPublicKeyFile string `yaml:"ssh_public_key_file"`
	// DataMount is where the persistent development disk is mounted.
	DataMount string `yaml:"data_mount"`
	// SwapGB sizes a swapfile the VM creates on boot so memory spikes degrade
	// into slowness instead of OOM-killing interactive sessions. 0 disables it.
	// Consumed only by Terraform (infra/locals.tf -> cloud-init); the Go CLI only
	// validates it. Pointer so an absent key defaults while an explicit 0
	// disables. Default 4 (applied in infra/locals.tf).
	SwapGB *int `yaml:"swap_gb"`
}

// Tailscale holds tailnet identity and policy configuration.
type Tailscale struct {
	Hostname  string `yaml:"hostname"`
	Tag       string `yaml:"tag"`
	SSHTarget string `yaml:"ssh_target"`

	// User is the tailnet identity (email or group) granted SSH access to the
	// workbox node. Used only to render the example policy fragment.
	User string `yaml:"user"`

	// ManagePolicy opts Terraform into owning the ENTIRE tailnet policy file.
	// Leave false unless this tailnet is dedicated to workbox. See docs/security.md.
	ManagePolicy bool `yaml:"manage_policy"`
}

// Schedule holds the baseline recurring schedule.
type Schedule struct {
	Timezone string `yaml:"timezone"`
	Wake     string `yaml:"wake"`
	Sleep    string `yaml:"sleep"`
}

// State holds Firestore operational-state locations.
type State struct {
	FirestoreDatabase string `yaml:"firestore_database"`
	// FirestoreLocation is where the database lives; it is set at creation time
	// by Terraform. Many compute regions (e.g. europe-north2) are not Firestore
	// locations, so this defaults to the eur3 European multi-region.
	FirestoreLocation string `yaml:"firestore_location"`
	Collection        string `yaml:"collection"`
	// Document defaults to Name when empty.
	Document string `yaml:"document"`
}

// ResolvePath returns the config path to load, honoring the precedence rules.
func ResolvePath(flagPath string) (string, error) {
	if flagPath != "" {
		return expandUser(flagPath)
	}
	if env := os.Getenv("WORKBOX_CONFIG"); env != "" {
		return expandUser(env)
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "workbox", "config.yaml"), nil
}

// Load reads, parses, defaults and validates the config at the resolved path.
func Load(flagPath string) (*Config, error) {
	path, err := ResolvePath(flagPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config not found at %s: run `make configure` to create it from workbox.example.yaml", path)
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	return Parse(data)
}

// Parse decodes, defaults and validates config bytes.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Defaults fills in optional fields that were left empty.
func (c *Config) Defaults() {
	if c.GCP.ImageFamily == "" {
		c.GCP.ImageFamily = "ubuntu-2404-lts-amd64"
	}
	if c.GCP.ImageProject == "" {
		c.GCP.ImageProject = "ubuntu-os-cloud"
	}
	if c.GCP.DeletionProtection == nil {
		v := true
		c.GCP.DeletionProtection = &v
	}
	if c.Machine.DataMount == "" {
		c.Machine.DataMount = "/work"
	}
	if c.State.FirestoreDatabase == "" {
		c.State.FirestoreDatabase = "(default)"
	}
	if c.State.FirestoreLocation == "" {
		c.State.FirestoreLocation = "eur3"
	}
	if c.State.Collection == "" {
		c.State.Collection = "workbox"
	}
	if c.State.Document == "" {
		c.State.Document = c.Name
	}
	if c.Tailscale.SSHTarget == "" {
		c.Tailscale.SSHTarget = c.Tailscale.Hostname
	}
}

// Validate checks required fields and cross-field constraints.
func (c *Config) Validate() error {
	var missing []string
	req := func(name, val string) {
		if strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	req("name", c.Name)
	req("gcp.project_id", c.GCP.ProjectID)
	req("gcp.zone", c.GCP.Zone)
	req("gcp.region", c.GCP.Region)
	req("gcp.machine_type", c.GCP.MachineType)
	req("machine.linux_user", c.Machine.LinuxUser)
	req("machine.ssh_public_key_file", c.Machine.SSHPublicKeyFile)
	req("tailscale.hostname", c.Tailscale.Hostname)
	req("schedule.timezone", c.Schedule.Timezone)
	req("schedule.wake", c.Schedule.Wake)
	req("schedule.sleep", c.Schedule.Sleep)
	if len(missing) > 0 {
		return fmt.Errorf("config is missing required fields: %s", strings.Join(missing, ", "))
	}
	if c.GCP.BootDiskGB <= 0 {
		return fmt.Errorf("gcp.boot_disk_gb must be positive")
	}
	if c.GCP.DataDiskGB <= 0 {
		return fmt.Errorf("gcp.data_disk_gb must be positive")
	}
	if c.Machine.SwapGB != nil && *c.Machine.SwapGB < 0 {
		return fmt.Errorf("machine.swap_gb must not be negative")
	}
	// Building the schedule validates timezone, HH:MM format and wake<sleep.
	if _, err := c.Schedule.Build(); err != nil {
		return err
	}
	return nil
}

// Build turns the schedule config into a schedule.Schedule.
func (s Schedule) Build() (schedule.Schedule, error) {
	return schedule.New(s.Wake, s.Sleep, s.Timezone)
}

// SSHPublicKeyPath returns the SSH public key path with ~ expanded.
func (m Machine) SSHPublicKeyPath() (string, error) {
	return expandUser(m.SSHPublicKeyFile)
}

// expandUser expands a leading ~ to the user's home directory.
func expandUser(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~ in %q: %w", p, err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}
