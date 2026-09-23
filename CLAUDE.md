# CLAUDE.md — workbox

Workbox turns a persistent GCP Compute Engine VM into a second computer. Running
`workbox` wakes the machine, waits for it to be reachable over Tailscale/SSH, and
opens a Herdr session against it. Other commands control power and scheduling.

## Architecture (two planes)

- **Control plane** (power/state): the Go CLI and a GCP Workflow call Google Cloud
  APIs directly to inspect/suspend the VM (only the CLI starts/resumes it) and to
  read/write a tiny Firestore state document. No `gcloud` shelling, no Terraform
  at runtime.
- **Connectivity plane**: local machine → Tailscale → OpenSSH → VM → Herdr/Claude.
  The VM has **no public inbound**; SSH arrives over Tailscale.

Terraform owns the *baseline desired infrastructure*. The CLI/Workflow own
*runtime operational state*. Keep that boundary: routine actions
(`wake`/`sleep`/`status`/`keep-awake`/`cancel`) must never run Terraform.

## Where things live

- `cmd/workbox/` — CLI entrypoint and Cobra wiring; connectivity flows
  (ssh/herdr/forward).
- `internal/schedule/` — the auto-suspend engine (working hours + idle timeout +
  holds/scheduled sleep). Pure, clock-injected, heavily tested. **This is the
  core; change it with tests.**
- `internal/config/` — the single YAML config schema (shared with Terraform).
- `internal/compute/` — `Compute` interface, GCP client, state model, fake, and
  the read-only `Activity` interface (last-active guest attribute and last
  instance start).
- `internal/state/` — Firestore-backed `Store`, the wire schema shared with the
  Workflow (`infra/reconcile.yaml.tftpl`), and a fake.
- `internal/cli/` — testable command logic (status/wake/sleep/keep-awake/cancel).
- `internal/ssh/`, `internal/herdr/` — process handoff (no Go SSH stack).
- `internal/doctor/` — read-only diagnostics.
- `infra/` — Terraform: APIs, VPC, VM, disks/snapshots, IAM, Firestore, Workflow,
  Scheduler, Tailscale. Templates: `cloud-init.sh.tftpl`, `reconcile.yaml.tftpl`.

## Auto-suspend model

**Wake is always manual; the VM suspends itself on idle.** The reconciler is a
once-a-minute, suspend-only watchdog — it never resumes/starts. It suspends a
RUNNING VM unless protected. Precedence: **scheduled sleep > keep-awake hold >
working hours > idle shutdown** (see `schedule.AutoSuspend`).

- **Working hours** (optional, `[start, end)` local time, optionally limited to
  certain weekdays via `days` — empty means every day; DST via the stdlib):
  auto-suspend disabled while inside; never wakes.
- **Idle shutdown** (default 30 min, `0` disables): outside working hours, suspend
  after inactivity. Activity = a herdr agent `working` **or** any open inbound SSH
  connection, reported by the on-VM emitter to a guest attribute
  (`workbox/last_active`). Idle counts from the later of that and the instance's
  `lastStartTimestamp` (boot grace for every start path).
- **Scheduled sleep** (`workbox sleep HH:MM`) and **keep-awake hold** are
  absolute-time spans in Firestore; `wake` sets a keep-awake grace (when idle
  shutdown is on) so a resumed VM isn't suspended before the emitter first
  reports.

Two contracts with the reconciler, each change-both-together:
- Firestore span layout: `internal/state/firestore.go` ↔
  `infra/reconcile.yaml.tftpl`.
- Activity guest-attribute key: `compute.LastActiveQueryPath` ↔
  `locals.activity_key` (emitter in `cloud-init.sh.tftpl`, read in
  `reconcile.yaml.tftpl`).

The engine (`schedule.AutoSuspend`) mirrors the reconciler; keep them in step.

## Canonical commands

```
workbox                 # wake, wait for SSH, open Herdr (default)
workbox status [--json] # VM state, working hours, activity, auto-suspend verdict
workbox wake            # resume now (+ keep-awake grace when idle shutdown is on)
workbox sleep [HH:MM]   # suspend now, or schedule a one-off suspend at HH:MM
workbox ssh | herdr     # wake, wait, hand off to ssh/herdr
workbox forward PORT... # wake, wait, hold local port-forwards to the VM open
workbox keep-awake 3h   # wake if needed, hold awake for a duration
workbox cancel          # clear scheduled sleep and keep-awake hold
workbox schedule        # working hours, idle setting, hold, scheduled sleep
workbox doctor          # read-only diagnostics
```

## Security boundaries (do not weaken)

- The **development VM must not receive broad cloud credentials**. It runs Claude
  Code, Docker and arbitrary tooling under a least-privilege SA (telemetry only).
- **Tailnet policy is global**: never overwrite it casually. Terraform owns it
  only when `tailscale.manage_policy` is explicitly true; otherwise the user
  applies a fragment by hand.
- **No secrets in Git**: no state, tfvars, keys, tokens, or Claude credentials.
  The single-use Tailscale enrollment key lives only in (git-ignored) state and
  its own `ignore_changes` instance metadata entry, so a replaced key never
  reaches a running VM.

## Working agreement

- Run `make check` before declaring work complete (gofmt, golangci-lint, test,
  build, terraform fmt/validate). It never touches cloud infrastructure.
  `golangci-lint` (v2) is a required tool — install it per the README before
  running `make check`/`make lint`.
- **Keep `README.md` in sync with user-facing behavior, in the same change.** Any
  change to the CLI command surface (`cmd/workbox`), the config schema
  (`internal/config` / `workbox.example.yaml`), the scheduling/auto-suspend model,
  or the setup flow must update the matching README section (CLI reference,
  Configuration, Daily usage, Architecture) as part of the same work — not later.
  Treat a stale README as a failing check.
- **Never** run `terraform apply`, `terraform destroy`, or any cloud-mutating
  command unless the human explicitly asks.
- Don't print or read credentials/secret material unnecessarily.
- When changing external resource schemas or APIs (GCP, Tailscale provider,
  Herdr, Claude Code install), verify against current official docs first.
- Prefer boring, explicit, testable, recoverable code. Put cloud calls behind
  interfaces so logic stays unit-testable.

Path-scoped detail lives in `.claude/rules/{go,terraform,security}.md`.
