# Architecture

Workbox has two independent planes. Keeping them separate is the central design
decision: how the machine is *controlled* (power and schedule) is deliberately
decoupled from how you *connect* to it.

## Control plane vs connectivity plane

```
CONTROL PLANE (power / state)                CONNECTIVITY PLANE (your session)

  workbox CLI ────┐                            your laptop
                  │                                 │
  Cloud Scheduler │  Google Cloud APIs             Tailscale (private tailnet)
        │         ▼                                 │
        ▼   ┌───────────────┐                       ▼
   Workflow │ Compute Engine│                     OpenSSH  ── key-only, no
   (reconciler)│  get/start/ │                       │        public ingress
        │    │ resume/suspend│                       ▼
        ▼    └───────────────┘                   workbox VM
   Firestore (tiny state doc)                        │
                                                      ▼
                                             Herdr server + panes
                                                      │
                                             Claude Code sessions
```

- **Control plane** — the Go CLI and a GCP Workflow call Google Cloud APIs
  directly (through the official Go client libraries and Workflows connectors;
  never by shelling out to `gcloud`). They inspect the instance, start/resume/
  suspend it, and read/write a small Firestore document. Cloud Scheduler triggers
  the Workflow once per minute.
- **Connectivity plane** — your machine reaches the VM only over Tailscale, then
  ordinary OpenSSH, then Herdr. The VM has a public egress IP for outbound
  package installs and Tailscale coordination, but **no inbound firewall rule**.

The two planes never cross: you can control power while the VM is unreachable
(suspended), and you connect without any cloud-admin credentials.

## Terraform owns infrastructure; the CLI owns runtime state

```
Terraform (infra/)             Go CLI + Workflow (runtime)
─────────────────────          ───────────────────────────
APIs, VPC, subnet              wake / sleep / status
VM, boot + data disks          wake-at / sleep-at / keep-awake
snapshot policy                cancel-override / schedule
IAM (3 SAs + operator role)    reconciler transitions
Firestore database             Firestore state document contents
Workflow + Scheduler
Tailscale key / policy
```

Terraform describes the *baseline desired infrastructure* and changes rarely.
Routine operation must **never** run Terraform — doing so would introduce drift
and couple a daily action to a heavyweight tool. To change the schedule
permanently you edit the config and run `make tf-apply`; everything else is an
API call.

## Suspend/Resume, not Stop/Start

The VM uses Compute Engine **Suspend/Resume** so that running processes, Herdr
panes and Claude Code sessions survive overnight. Suspend writes RAM to disk;
resume restores it.

`workbox wake` chooses the right transition from the current state and is
idempotent:

| State (GCP)        | Meaning                       | `wake` does | `sleep` does |
|--------------------|-------------------------------|-------------|--------------|
| `RUNNING`          | up                            | nothing     | suspend      |
| `SUSPENDED`        | RAM preserved on disk         | resume      | nothing      |
| `TERMINATED`       | stopped, no preserved memory  | start       | nothing      |
| `SUSPENDING`/`STOPPING`/`PROVISIONING`/`STAGING` | transitional | wait, re-evaluate | wait, re-evaluate |
| `REPAIRING`        | under repair                  | report      | report       |
| unrecognized (`UNKNOWN`) | non-transitional, non-stable | report | report |

There is **no `STOPPED`** state in GCP — a stopped instance is `TERMINATED`. The
code models these explicitly in `internal/compute/state.go` rather than passing
raw strings around. Any status that is neither transitional nor stable —
`REPAIRING` or an unrecognized status normalized to `UNKNOWN` — is reported as
unsupported (`ErrUnsupportedState`) rather than waited on, so `wake`/`sleep` fail
fast instead of polling a stuck instance indefinitely.

Suspend has a documented ceiling: after **60 days** suspended, Compute Engine
auto-transitions the instance to `TERMINATED` and the preserved memory is lost
(disks persist). Because `wake` handles both `SUSPENDED → resume` and
`TERMINATED → start`, this degrades gracefully to a normal boot. See
[operations.md](operations.md).

## Scheduling architecture

```
Cloud Scheduler  ──(every minute, OAuth)──▶  Workflow reconciler
                                                   │
                     reads baseline schedule ◀─────┤
                     reads override/hold spans ◀────┼──  Firestore state doc
                     gets instance status ◀─────────┤
                     performs one transition ────────▶  Compute Engine API
```

A GCP **Workflow** is the reconciler rather than an always-on `workboxd` daemon:
there is nothing to keep running, patch, or pay for between ticks. Each run:

1. gets the current time in the configured IANA timezone via `time.format`
   (DST-correct, no manual offsets);
2. derives the baseline desired state from the normal schedule;
3. reads override/hold spans from Firestore;
4. applies them in precedence order (below);
5. gets the instance status;
6. performs only the single transition required (resume/start/suspend), or
   nothing;
7. deletes the state document once every span has expired.

It is idempotent: if the state already matches the desired state, it does
nothing.

### Approximate cost of the reconciler

Running once per minute (~43,200 executions/month):

- **Cloud Scheduler** — within the 3-free-jobs tier → **$0**.
- **Firestore reads** — ~1,440/day, far under the free daily allowance → **$0**.
- **Workflows** — billed per internal step; all Compute/Firestore calls are
  internal steps → roughly **$3–$8/month**, driven by step count per run (most
  ticks are read-only no-ops).

Total ≈ **$3–8/month** for the scheduler, on top of the VM's own compute (only
while `RUNNING`) and disk/snapshot storage.

## Operational state document

One tiny Firestore document (collection `workbox`, id = instance name) holds the
scheduling overrides. Its field layout is the contract between
`internal/state/firestore.go` and `infra/reconcile.yaml.tftpl`:

```
wake_override:  { start: <timestamp>, end: <timestamp>, state: "awake"|"asleep" }
sleep_override: { start: <timestamp>, end: <timestamp>, state: "awake"|"asleep" }
hold:           { start: <timestamp>, end: <timestamp>, state: "awake"|"asleep" }
updated_by:     <string>
updated_at:     <timestamp>
```

Each is an optional half-open span `[start, end)`. **Precedence, lowest to
highest:**

```
baseline recurring schedule
    < one-workday override (wake_override / sleep_override)
    < manual hold
```

Overrides are one-shot: they cover exactly one boundary and expire when `now`
passes their `end`. See the schedule engine in `internal/schedule/schedule.go`
and the worked examples in [operations.md](operations.md).

## Persistent development disk

The development data lives on a **separate** disk from the OS so it survives VM
replacement and reprovisioning:

- boot disk: 30 GB, `auto_delete = true`;
- data disk: 200 GB `pd-balanced`, `auto_delete = false`, Terraform
  `prevent_destroy`.

The disk is attached with a stable `device_name`, so the guest mounts it by a
stable path (`/dev/disk/by-id/google-workbox-data`) — never a brittle `/dev/sdb`.
The bootstrap script formats it **only if it has no filesystem**, adds an
`fstab` entry by UUID (so it returns automatically after reboots/resumes), mounts
it at `/work`, and creates `/work/projects` and `/work/docker`. `~/work` is a
symlink to `/work/projects`, and Docker's data-root is `/work/docker`, so
containers and images also live on the persistent disk. Detach/reattach steps are
in [operations.md](operations.md).
