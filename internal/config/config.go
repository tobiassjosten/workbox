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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	SSH       SSH       `yaml:"ssh"`
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

// Schedule holds the timezone, the optional working-hours protection window, and
// the idle-shutdown timeout.
type Schedule struct {
	// Timezone is the IANA timezone for working hours and the VM clock.
	Timezone string `yaml:"timezone"`
	// WorkingHours is the optional protected window; nil means no window.
	WorkingHours *WorkingHours `yaml:"working_hours"`
	// IdleTimeoutMinutes is how long the VM may be inactive outside working hours
	// before auto-suspend. A pointer so an absent key defaults to
	// defaultIdleTimeoutMinutes while an explicit 0 disables idle shutdown.
	IdleTimeoutMinutes *int `yaml:"idle_timeout_minutes"`
}

// WorkingHours is a recurring window, optionally limited to certain weekdays,
// during which idle shutdown is disabled.
type WorkingHours struct {
	// Enabled defaults to true when the block is present; set false to keep the
	// window configured but inactive.
	Enabled *bool  `yaml:"enabled"`
	Start   string `yaml:"start"`
	End     string `yaml:"end"`
	// Days restricts the window to these weekdays (e.g. [mon, tue, wed, thu, fri]).
	// Names are case-insensitive: 3-letter, full, or the common abbreviations in
	// weekdayNames (tues, weds, thur, thurs). Empty means every day.
	Days []string `yaml:"days"`
}

// weekdayNames maps accepted day spellings to time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// Weekdays parses Days into distinct time.Weekday values (empty stays empty =
// every day); duplicates such as [mon, monday] collapse to one.
func (w *WorkingHours) Weekdays() ([]time.Weekday, error) {
	if w == nil || len(w.Days) == 0 {
		return nil, nil
	}
	out := make([]time.Weekday, 0, len(w.Days))
	for _, name := range w.Days {
		wd, ok := weekdayNames[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return nil, fmt.Errorf("schedule.working_hours.days: unknown day %q "+
				"(use names like mon, tue or monday, tuesday)", name)
		}
		if !slices.Contains(out, wd) {
			out = append(out, wd)
		}
	}
	return out, nil
}

// Active reports whether the working-hours window is present and enabled.
func (w *WorkingHours) Active() bool {
	return w != nil && (w.Enabled == nil || *w.Enabled)
}

// Firestore document defaults (database and collection), mirrored in
// infra/locals.tf: the CLI and the reconciler must target the same document.
const (
	defaultFirestoreDatabase = "(default)"
	defaultStateCollection   = "workbox"
)

// defaultIdleTimeoutMinutes is used when idle_timeout_minutes is absent.
const defaultIdleTimeoutMinutes = 30

// IdleTimeout returns the configured idle-shutdown duration (0 disables it).
func (s Schedule) IdleTimeout() time.Duration {
	m := defaultIdleTimeoutMinutes
	if s.IdleTimeoutMinutes != nil {
		m = *s.IdleTimeoutMinutes
	}
	return time.Duration(m) * time.Minute
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

// SSH tunes how the CLI probes for and waits on SSH reachability after a wake.
// The defaults suit a healthy VM; raise them if a loaded machine is slow to
// complete the SSH handshake (which otherwise shows up as "Waiting for SSH..."
// timing out after WaitTimeoutSeconds).
type SSH struct {
	// ConnectTimeoutSeconds bounds each reachability probe's SSH handshake.
	// Pointer so an absent key defaults while an explicit value (incl. small
	// ones) is honored. Default defaultSSHConnectTimeoutSeconds.
	ConnectTimeoutSeconds *int `yaml:"connect_timeout_seconds"`
	// WaitTimeoutSeconds is the overall budget for waiting for SSH after a wake
	// before giving up with a diagnostic. Default defaultSSHWaitTimeoutSeconds.
	WaitTimeoutSeconds *int `yaml:"wait_timeout_seconds"`
}

const (
	defaultSSHConnectTimeoutSeconds = 15
	defaultSSHWaitTimeoutSeconds    = 180
)

// ConnectTimeout returns the per-probe SSH connect timeout.
func (s SSH) ConnectTimeout() time.Duration {
	n := defaultSSHConnectTimeoutSeconds
	if s.ConnectTimeoutSeconds != nil {
		n = *s.ConnectTimeoutSeconds
	}
	return time.Duration(n) * time.Second
}

// WaitTimeout returns the overall wait-for-SSH budget after a wake.
func (s SSH) WaitTimeout() time.Duration {
	n := defaultSSHWaitTimeoutSeconds
	if s.WaitTimeoutSeconds != nil {
		n = *s.WaitTimeoutSeconds
	}
	return time.Duration(n) * time.Second
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

// minIdleTimeoutMinutes is the shortest accepted idle timeout. The on-VM
// emitter reports about once a minute (up to ~75 s apart) and the reconciler
// ticks once a minute, so a shorter timeout could suspend a VM in use. Mirrored
// by the precondition in infra/workflow.tf.
const minIdleTimeoutMinutes = 5

// errLegacySchedule explains the retired schedule.wake/schedule.sleep keys; the
// Terraform precondition in infra/workflow.tf reports the same migration.
var errLegacySchedule = errors.New("schedule.wake/schedule.sleep were replaced by schedule.working_hours.start/end; " +
	"migrate your config, then follow docs/operations.md (\"Upgrading from schedule.wake/sleep\") before `make tf-apply`")

// Parse decodes, defaults and validates config bytes.
func Parse(data []byte) (*Config, error) {
	// Catch the retired schedule.wake/sleep keys before strict decoding, which
	// would only report a generic unknown-field error.
	var legacy struct {
		Schedule struct {
			Wake  *string `yaml:"wake"`
			Sleep *string `yaml:"sleep"`
		} `yaml:"schedule"`
	}
	if err := yaml.Unmarshal(data, &legacy); err == nil &&
		(legacy.Schedule.Wake != nil || legacy.Schedule.Sleep != nil) {
		return nil, errLegacySchedule
	}

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
		c.State.FirestoreDatabase = defaultFirestoreDatabase
	}
	if c.State.FirestoreLocation == "" {
		c.State.FirestoreLocation = "eur3"
	}
	if c.State.Collection == "" {
		c.State.Collection = defaultStateCollection
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
	if len(missing) > 0 {
		return fmt.Errorf("config is missing required fields: %s", strings.Join(missing, ", "))
	}
	if c.GCP.BootDiskGB <= 0 {
		return errors.New("gcp.boot_disk_gb must be positive")
	}
	if c.GCP.DataDiskGB <= 0 {
		return errors.New("gcp.data_disk_gb must be positive")
	}
	if n := c.Schedule.IdleTimeoutMinutes; n != nil && *n != 0 && *n < minIdleTimeoutMinutes {
		return fmt.Errorf("schedule.idle_timeout_minutes must be 0 (disabled) or at least %d", minIdleTimeoutMinutes)
	}
	if c.Machine.SwapGB != nil && *c.Machine.SwapGB < 0 {
		return errors.New("machine.swap_gb must not be negative")
	}
	if c.SSH.ConnectTimeoutSeconds != nil && *c.SSH.ConnectTimeoutSeconds <= 0 {
		return errors.New("ssh.connect_timeout_seconds must be positive")
	}
	if c.SSH.WaitTimeoutSeconds != nil && *c.SSH.WaitTimeoutSeconds <= 0 {
		return errors.New("ssh.wait_timeout_seconds must be positive")
	}
	// Day names are checked even for a disabled window, as Terraform does.
	if _, err := c.Schedule.WorkingHours.Weekdays(); err != nil {
		return err
	}
	// Building the schedule validates the timezone and, when working hours are
	// enabled, the HH:MM format and start<end ordering.
	if _, err := c.Schedule.Build(); err != nil {
		return err
	}
	return nil
}

// Build turns the schedule config into a schedule.Schedule. When working hours
// are absent or disabled the result is a window-less (idle-only) schedule.
func (s Schedule) Build() (schedule.Schedule, error) {
	if s.WorkingHours.Active() {
		days, err := s.WorkingHours.Weekdays()
		if err != nil {
			return schedule.Schedule{}, err
		}
		return schedule.New(s.WorkingHours.Start, s.WorkingHours.End, s.Timezone, days...)
	}
	return schedule.Disabled(s.Timezone)
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
