# CLAUDE.md — workbox

Workbox turns a persistent GCP Compute Engine VM into a second computer. Running
`workbox` wakes the machine, waits for it to be reachable over Tailscale/SSH, and
opens a Herdr session against it. Other commands control power and scheduling.

## Architecture (two planes)

- **Control plane** (power/state): the Go CLI and a GCP Workflow call Google Cloud
  APIs directly to inspect/start/resume/suspend the VM and to read/write a tiny
  Firestore state document. No `gcloud` shelling, no Terraform at runtime.
- **Connectivity plane**: local machine → Tailscale → OpenSSH → VM → Herdr/Claude.
  The VM has **no public inbound**; SSH arrives over Tailscale.

Terraform owns the *baseline desired infrastructure*. The CLI/Workflow own
*runtime operational state*. Keep that boundary: routine actions
(`wake`/`sleep`/`status`/overrides) must never run Terraform.

## Where things live

- `cmd/workbox/` — CLI entrypoint and Cobra wiring; connectivity flows (ssh/herdr).
- `internal/schedule/` — the schedule engine (baseline + overrides + holds). Pure,
  clock-injected, heavily tested. **This is the core; change it with tests.**
- `internal/config/` — the single YAML config schema (shared with Terraform).
- `internal/compute/` — `Compute` interface, GCP client, state model, fake.
- `internal/state/` — Firestore-backed `Store`, the wire schema shared with the
  Workflow (`infra/reconcile.yaml.tftpl`), and a fake.
- `internal/cli/` — testable command logic (status/wake/sleep/overrides).
- `internal/ssh/`, `internal/herdr/` — process handoff (no Go SSH stack).
- `internal/doctor/` — read-only diagnostics.
- `infra/` — Terraform: APIs, VPC, VM, disks/snapshots, IAM, Firestore, Workflow,
  Scheduler, Tailscale. Templates: `cloud-init.sh.tftpl`, `reconcile.yaml.tftpl`.

## Scheduling model

Baseline: awake in `[wake, sleep)` local time, every day, in the configured IANA
timezone (DST via the stdlib, never fixed offsets). Overrides are absolute-time
spans stored in Firestore; precedence is **manual hold > one-workday override >
baseline**. Overrides are one-shot and expire by time. `wake`/`sleep` set a
temporary hold so manual actions do not fight the reconciler. See
`internal/schedule/schedule.go` and `docs/operations.md`.

The same Firestore document is read by the Workflow reconciler; its field layout
is the contract in `internal/state/firestore.go` ↔ `infra/reconcile.yaml.tftpl`.
Change both together.

## Canonical commands

```
workbox                 # wake, wait for SSH, open Herdr (default)
workbox status [--json] # VM + schedule state
workbox wake | sleep    # power now, with hold semantics
workbox ssh | herdr     # wake, wait, hand off to ssh/herdr
workbox wake-at HH:MM    # one-workday wake override
workbox sleep-at HH:MM   # one-workday sleep override (handles past-midnight)
workbox keep-awake 3h    # hold awake for a duration
workbox cancel-override  # clear overrides/holds
workbox schedule         # schedule, overrides, holds, next transitions
workbox doctor           # read-only diagnostics
```

## Security boundaries (do not weaken)

- The **development VM must not receive broad cloud credentials**. It runs Claude
  Code, Docker and arbitrary tooling under a least-privilege SA (telemetry only).
- **Tailnet policy is global**: never overwrite it casually. Terraform owns it
  only when `tailscale.manage_policy` is explicitly true; otherwise the user
  applies a fragment by hand.
- **No secrets in Git**: no state, tfvars, keys, tokens, or Claude credentials.
  The single-use Tailscale enrollment key lives only in (git-ignored) state.

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
