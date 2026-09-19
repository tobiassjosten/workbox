// Command workbox controls a persistent GCP development VM: it manages the
// machine's power state through Google Cloud APIs (the control plane) and opens
// a Herdr session over Tailscale/SSH (the connectivity plane).
//
// Routine operations never run Terraform. Terraform owns the baseline
// infrastructure; this CLI owns runtime state.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tobiassjosten/workbox/internal/cli"
	"github.com/tobiassjosten/workbox/internal/compute"
	"github.com/tobiassjosten/workbox/internal/config"
	"github.com/tobiassjosten/workbox/internal/doctor"
	"github.com/tobiassjosten/workbox/internal/herdr"
	"github.com/tobiassjosten/workbox/internal/ssh"
	"github.com/tobiassjosten/workbox/internal/state"
)

// Version is overridable at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "workbox: %s\n", err)
		os.Exit(1)
	}
}

// signalContext returns a context cancelled on SIGINT/SIGTERM so long waits and
// in-flight operations exit cleanly on Ctrl-C.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newRootCmd() *cobra.Command {
	var cfgPath string
	var jsonOut bool

	// runUpCmd bypasses appCmd because the up/herdr flow replaces the process
	// via syscall.Exec rather than returning through normal cleanup.
	runUpCmd := func(_ *cobra.Command, _ []string) error {
		ctx, cancel := signalContext()
		defer cancel()
		return runUp(ctx, cfgPath)
	}

	root := &cobra.Command{
		Use:           "workbox",
		Short:         "Control a persistent GCP development VM that feels like a second computer",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Default command: wake, wait for reachability, open Herdr.
		RunE: runUpCmd,
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "",
		"path to config (default $WORKBOX_CONFIG or ~/.config/workbox/config.yaml)")

	// status
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show the current VM and schedule state",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			app, cleanup, err := newApp(ctx, cfgPath)
			if err != nil {
				return err
			}
			defer cleanup()
			if jsonOut {
				return app.PrintStatusJSON(ctx)
			}
			return app.PrintStatus(ctx)
		},
	}
	statusCmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	root.AddCommand(statusCmd)

	// wake
	root.AddCommand(&cobra.Command{
		Use:   "wake",
		Short: "Resume/start the VM now (holds it awake if the schedule wants it asleep)",
		RunE:  appCmd(&cfgPath, func(ctx context.Context, a *cli.App, _ []string) error { return a.Wake(ctx) }),
	})

	// sleep (+ alias off)
	root.AddCommand(&cobra.Command{
		Use:     "sleep",
		Aliases: []string{"off"},
		Short:   "Suspend the VM now (holds it asleep if the schedule wants it awake)",
		RunE:    appCmd(&cfgPath, func(ctx context.Context, a *cli.App, _ []string) error { return a.Sleep(ctx) }),
	})

	// ssh
	root.AddCommand(&cobra.Command{
		Use:                "ssh [-- ssh args...]",
		Short:              "Wake if needed, wait for SSH, then hand off to your ssh client",
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runSSH(ctx, cfgPath, args)
		},
	})

	// herdr
	root.AddCommand(&cobra.Command{
		Use:   "herdr",
		Short: "Wake if needed, wait for SSH, then open Herdr attached to the VM",
		RunE:  runUpCmd,
	})

	// wake-at
	root.AddCommand(&cobra.Command{
		Use:   "wake-at HH:MM",
		Short: "Set a one-workday override for the next wake transition",
		Args:  cobra.ExactArgs(1),
		RunE:  appCmd(&cfgPath, func(ctx context.Context, a *cli.App, args []string) error { return a.WakeAt(ctx, args[0]) }),
	})

	// sleep-at
	root.AddCommand(&cobra.Command{
		Use:   "sleep-at HH:MM",
		Short: "Set a one-workday override for the current/next sleep transition",
		Args:  cobra.ExactArgs(1),
		RunE:  appCmd(&cfgPath, func(ctx context.Context, a *cli.App, args []string) error { return a.SleepAt(ctx, args[0]) }),
	})

	// keep-awake
	root.AddCommand(&cobra.Command{
		Use:   "keep-awake DURATION",
		Short: "Keep the VM awake for at least DURATION (e.g. 3h, 90m)",
		Args:  cobra.ExactArgs(1),
		RunE: appCmd(&cfgPath, func(ctx context.Context, a *cli.App, args []string) error {
			d, err := time.ParseDuration(args[0])
			if err != nil {
				return fmt.Errorf("invalid duration %q: %w", args[0], err)
			}
			return a.KeepAwake(ctx, d)
		}),
	})

	// cancel-override
	root.AddCommand(&cobra.Command{
		Use:   "cancel-override",
		Short: "Remove active one-workday overrides and manual holds",
		RunE:  appCmd(&cfgPath, func(ctx context.Context, a *cli.App, _ []string) error { return a.CancelOverride(ctx) }),
	})

	// schedule
	root.AddCommand(&cobra.Command{
		Use:   "schedule",
		Short: "Show the normal schedule, overrides, holds and next transitions",
		RunE:  appCmd(&cfgPath, func(ctx context.Context, a *cli.App, _ []string) error { return a.Schedule(ctx) }),
	})

	// doctor
	var doctorWake bool
	doctorCmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run diagnostics; use --wake to first wake the VM for remote checks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runDoctor(ctx, cfgPath, doctorWake)
		},
	}
	doctorCmd.Flags().BoolVar(&doctorWake, "wake", false,
		"wake the VM first so the remote SSH/Herdr/Claude checks can run")
	root.AddCommand(doctorCmd)

	return root
}

// appCmd adapts an App method into a cobra RunE that wires config, compute and
// state, and cleans up afterward.
func appCmd(cfgPath *string, fn func(ctx context.Context, a *cli.App, args []string) error) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		ctx, cancel := signalContext()
		defer cancel()
		app, cleanup, err := newApp(ctx, *cfgPath)
		if err != nil {
			return err
		}
		defer cleanup()
		return fn(ctx, app, args)
	}
}

// newApp loads config and builds a fully wired App.
func newApp(ctx context.Context, cfgPath string) (*cli.App, func(), error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	return buildApp(ctx, cfg)
}

// buildApp builds a fully wired App from an already-loaded config.
func buildApp(ctx context.Context, cfg *config.Config) (*cli.App, func(), error) {
	sc, err := cfg.Schedule.Build()
	if err != nil {
		return nil, nil, err
	}
	comp, err := compute.NewGCP(ctx, compute.GCPConfig{
		Project:  cfg.GCP.ProjectID,
		Zone:     cfg.GCP.Zone,
		Instance: cfg.Name,
	})
	if err != nil {
		return nil, nil, err
	}
	store, err := state.NewFirestore(ctx, state.FirestoreConfig{
		Project:    cfg.GCP.ProjectID,
		Database:   cfg.State.FirestoreDatabase,
		Collection: cfg.State.Collection,
		Document:   cfg.State.Document,
		Updater:    currentUser(),
	})
	if err != nil {
		_ = comp.Close() // best-effort cleanup; return the original error
		return nil, nil, err
	}
	app := &cli.App{
		Cfg:     cfg,
		Sched:   sc,
		Compute: comp,
		Store:   store,
		Out:     os.Stdout,
	}
	cleanup := func() {
		// Best-effort release of client connections on shutdown; a close error is
		// not actionable here and must not mask the command's own result.
		_ = comp.Close()
		_ = store.Close()
	}
	return app, cleanup, nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "workbox-cli"
}

// sshTarget builds the ssh.Target from config.
func sshTarget(cfg *config.Config) ssh.Target {
	return ssh.Target{Host: cfg.Tailscale.SSHTarget, User: cfg.Machine.LinuxUser}
}

// wakeAndWait wakes the VM (with a stay-awake hold if needed) and waits for SSH.
func wakeAndWait(ctx context.Context, cfg *config.Config) error {
	const (
		waitTimeout  = 3 * time.Minute
		waitInterval = 3 * time.Second
	)
	app, cleanup, err := buildApp(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := app.Wake(ctx); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Waiting for SSH...")
	return ssh.WaitReachable(ctx, sshTarget(cfg), waitTimeout, waitInterval)
}

// runUp is the default flow: wake, wait, open Herdr.
func runUp(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := wakeAndWait(ctx, cfg); err != nil {
		return err
	}
	return herdr.Exec(sshTarget(cfg).Destination())
}

// runSSH wakes, waits, then replaces the process with ssh.
func runSSH(ctx context.Context, cfgPath string, extra []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := wakeAndWait(ctx, cfg); err != nil {
		return err
	}
	return ssh.Exec(sshTarget(cfg), extra...)
}

// runDoctor runs read-only diagnostics. With wake=true it first wakes the VM and
// waits for SSH so the remote checks can run; otherwise it never wakes it.
func runDoctor(ctx context.Context, cfgPath string, wake bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// Still report the config failure through the doctor format.
		remedy := "run `make configure` to regenerate the config file"
		if path, perr := config.ResolvePath(cfgPath); perr == nil {
			if _, serr := os.Stat(path); serr == nil {
				// File exists but failed to parse or validate — editing is the fix.
				remedy = "edit " + path + " to fix the error above"
			}
		}
		printResults(os.Stdout, []doctor.Result{{
			Name: "config", Level: doctor.Fail, Detail: err.Error(),
			Remedy: remedy,
		}})
		return doctor.ErrChecksFailed
	}

	if wake {
		if err := wakeAndWait(ctx, cfg); err != nil {
			return err
		}
	}

	deps := doctor.Deps{Config: cfg, Target: sshTarget(cfg)}
	if comp, e := compute.NewGCP(ctx, compute.GCPConfig{
		Project: cfg.GCP.ProjectID, Zone: cfg.GCP.Zone, Instance: cfg.Name,
	}); e != nil {
		deps.ComputeErr = e
	} else {
		defer func() { _ = comp.Close() }() // best-effort; nothing actionable on failure
		deps.Compute = comp
	}
	results := doctor.Run(ctx, deps)
	printResults(os.Stdout, results)
	if doctor.Failed(results) {
		return doctor.ErrChecksFailed
	}
	return nil
}

func printResults(w io.Writer, results []doctor.Result) {
	for _, r := range results {
		fmt.Fprintf(w, "[%-4s] %-24s %s\n", r.Level, r.Name, r.Detail)
		if r.Remedy != "" && (r.Level == doctor.Fail || r.Level == doctor.Warn) {
			fmt.Fprintf(w, "         ↳ %s\n", r.Remedy)
		}
	}
}
