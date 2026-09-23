// Command workbox controls a persistent GCP development VM: it manages the
// machine's power state through Google Cloud APIs (the control plane) and opens
// a Herdr session over Tailscale/SSH (the connectivity plane).
//
// Routine operations never run Terraform. Terraform owns the baseline
// infrastructure; this CLI owns runtime state.
package main

import (
	"context"
	"errors"
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
		Short: "Show the VM state, working hours, activity and the auto-suspend verdict",
		RunE: appCmd(&cfgPath, func(ctx context.Context, a *cli.App, _ []string) error {
			if jsonOut {
				return a.PrintStatusJSON(ctx)
			}
			return a.PrintStatus(ctx)
		}),
	}
	statusCmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	root.AddCommand(statusCmd)

	// wake
	root.AddCommand(&cobra.Command{
		Use:   "wake",
		Short: "Resume/start the VM now (with a short keep-awake grace when idle shutdown is on)",
		RunE:  appCmd(&cfgPath, func(ctx context.Context, a *cli.App, _ []string) error { return a.Wake(ctx) }),
	})

	// sleep [HH:MM] (+ alias off)
	root.AddCommand(&cobra.Command{
		Use:     "sleep [HH:MM]",
		Aliases: []string{"off"},
		Short:   "Suspend now, or schedule a one-off suspend at HH:MM (or HHMM, configured timezone)",
		Args:    cobra.MaximumNArgs(1),
		RunE:    appCmd(&cfgPath, runSleep),
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

	// forward
	root.AddCommand(&cobra.Command{
		Use:   "forward PORT [PORT...]",
		Short: "Wake if needed, wait for SSH, then hold local port-forwards to the VM open",
		Long: "Tunnel one or more local ports to services on the VM's loopback so you can\n" +
			"review them in a local browser (e.g. `workbox forward 1313` for `hugo serve`).\n" +
			"Each PORT is either \"PORT\" (same port both ends) or \"LOCAL:REMOTE\". The VM\n" +
			"side is always localhost, so nothing is exposed beyond the tunnel. The session\n" +
			"holds the forwards open with no remote shell; press Ctrl-C to close them.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runForward(ctx, cfgPath, args)
		},
	})

	// keep-awake
	root.AddCommand(&cobra.Command{
		Use:   "keep-awake DURATION",
		Short: "Wake if needed, then keep the VM awake for at least DURATION (e.g. 3h, 90m)",
		Args:  cobra.ExactArgs(1),
		RunE: appCmd(&cfgPath, func(ctx context.Context, a *cli.App, args []string) error {
			d, err := time.ParseDuration(args[0])
			if err != nil {
				return fmt.Errorf("invalid duration %q: %w", args[0], err)
			}
			return a.KeepAwake(ctx, d)
		}),
	})

	// cancel
	root.AddCommand(&cobra.Command{
		Use:   "cancel",
		Short: "Clear the scheduled sleep and keep-awake hold",
		RunE:  appCmd(&cfgPath, func(ctx context.Context, a *cli.App, _ []string) error { return a.Cancel(ctx) }),
	})

	// The retired commands explain themselves rather than failing with a bare
	// "unknown command" (or, for sleep-at, a suggestion that would suspend the
	// VM immediately if followed literally).
	for _, retired := range []struct{ use, msg string }{
		{"wake-at", "wake-at was removed: waking is manual now (`workbox wake`); " +
			"auto-suspend is paused during schedule.working_hours (if configured), " +
			"and the VM otherwise suspends when idle"},
		{"sleep-at", "sleep-at was replaced by `workbox sleep HH:MM` " +
			"(bare `workbox sleep` suspends immediately)"},
		{"cancel-override", "cancel-override was renamed to `workbox cancel`"},
	} {
		root.AddCommand(&cobra.Command{
			Use:    retired.use,
			Short:  retired.msg,
			Hidden: true,
			Args:   cobra.ArbitraryArgs,
			RunE:   func(_ *cobra.Command, _ []string) error { return errors.New(retired.msg) },
		})
	}

	// schedule
	root.AddCommand(&cobra.Command{
		Use:   "schedule",
		Short: "Show working hours, idle shutdown, keep-awake hold and scheduled sleep",
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

// runSleep suspends now, or schedules a one-off suspend when given an HH:MM.
func runSleep(ctx context.Context, a *cli.App, args []string) error {
	if len(args) == 1 {
		return a.SleepAt(ctx, args[0])
	}
	return a.Sleep(ctx)
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
		Cfg:      cfg,
		Sched:    sc,
		Compute:  comp,
		Store:    store,
		Activity: comp, // *compute.GCP reads the last-active guest attribute and lastStartTimestamp
		Out:      os.Stdout,
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

// wakeAndWait wakes the VM (see cli.App.Wake for the post-wake grace hold) and
// waits for SSH.
func wakeAndWait(ctx context.Context, cfg *config.Config) error {
	const waitInterval = 3 * time.Second
	connectTimeout := cfg.SSH.ConnectTimeout()
	waitTimeout := cfg.SSH.WaitTimeout()

	app, cleanup, err := buildApp(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	// This flow hands off to ssh/herdr, whose stdout is the remote command's
	// output; keep workbox's own progress lines on stderr.
	app.Out = os.Stderr

	if err := app.Wake(ctx); err != nil {
		return err
	}
	target := sshTarget(cfg)
	fmt.Fprintln(os.Stderr, "Waiting for SSH...")
	if err := ssh.WaitReachable(ctx, target, connectTimeout, waitTimeout, waitInterval); err != nil {
		return diagnoseUnreachable(ctx, os.Stderr, app.Compute, target, waitTimeout, err)
	}
	return nil
}

// diagnoseUnreachable turns a bare wait-timeout into an actionable message
// unless the VM is known to be asleep. The wait error (printed by the caller)
// carries ssh's last error, which names the cause; the hint lists the usual ones
// and how to dig further. A cancelled wait (Ctrl-C) passes through with no hint,
// as does a VM the Compute API reports in any state but RUNNING — an unreadable
// state still gets the hint, since that is when the user needs it most.
func diagnoseUnreachable(ctx context.Context, w io.Writer, comp compute.Compute, target ssh.Target, waitTimeout time.Duration, waitErr error) error {
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		return waitErr
	}
	st, statusErr := comp.Status(ctx)
	if statusErr != nil || st == compute.Running {
		// The error text is an argument, never part of the format: a % in it
		// would otherwise be read as a verb and garble the whole hint.
		first := "The VM reports RUNNING but SSH did not become reachable within %s.\n"
		args := []any{waitTimeout}
		if statusErr != nil {
			first = "SSH did not become reachable within %s, and the VM's state could not be read (%v).\n"
			args = append(args, statusErr)
		}
		args = append(args, target.Destination())
		fmt.Fprintf(w,
			"\n"+first+
				"If an ssh error is shown below, it usually names the cause. Common ones:\n"+
				"  - local Tailscale down, or the host name not resolving\n"+
				"  - a changed host key (e.g. after a VM rebuild): check ~/.ssh/known_hosts\n"+
				"  - no usable key in your ssh-agent (Permission denied)\n"+
				"  - an overloaded VM whose SSH handshake is too slow for the probe\n"+
				"Connect directly for details (and, if overloaded, to shed load):\n"+
				"    ssh -v -o ConnectTimeout=60 -o ServerAliveInterval=15 %s\n"+
				"For a slow VM, raise ssh.connect_timeout_seconds / ssh.wait_timeout_seconds in your config.\n\n",
			args...)
	}
	return waitErr
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

// runForward wakes, waits, then replaces the process with a non-interactive ssh
// session that holds one or more local port-forwards open. It lets the user
// review a dev server bound to the VM's loopback in a local browser without
// exposing any port beyond the tunnel; Ctrl-C closes the forwards.
func runForward(ctx context.Context, cfgPath string, specs []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	forwards, err := ssh.ParseForwards(specs)
	if err != nil {
		return err
	}
	// Fail on a busy local port before waking (and billing) the VM.
	if err := ssh.CheckLocalPorts(forwards); err != nil {
		return err
	}
	if err := wakeAndWait(ctx, cfg); err != nil {
		return err
	}
	for _, f := range forwards {
		fmt.Fprintf(os.Stderr, "Forwarding localhost:%d -> VM localhost:%d\n", f.Local, f.Remote)
	}
	fmt.Fprintln(os.Stderr, "Holding forwards open; press Ctrl-C to close.")
	return ssh.ExecForward(sshTarget(cfg), forwards)
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

	// A failed wake is reported as a check rather than returned: the Cloud-API
	// checks need no SSH, and "I woke it and something is wrong" is exactly when
	// they are wanted. Ctrl-C still aborts.
	var wakeResult []doctor.Result
	if wake {
		if err := wakeAndWait(ctx, cfg); err != nil {
			if ctx.Err() != nil {
				return err
			}
			wakeResult = []doctor.Result{{
				Name: "wake", Level: doctor.Fail, Detail: err.Error(),
				Remedy: "see the diagnosis above; the checks below ran anyway",
			}}
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
		deps.Activity = comp
	}
	results := append(wakeResult, doctor.Run(ctx, deps)...)
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
