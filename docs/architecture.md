# Architecture

Workbox has two independent planes. Keeping them separate is the central design
decision: how the machine is *controlled* (power and schedule) is deliberately
decoupled from how you *connect* to it.

## Control plane vs connectivity plane

```
CONTROL PLANE (power / state)                CONNECTIVITY PLANE (your session)

  workbox CLI ──────▶ Compute Engine         your laptop
    │   get/start/resume/suspend                    │
    ▼                                               ▼
  Firestore (tiny state doc)                 Tailscale (private tailnet)
    ▲                                               │
    │                                               ▼
  Workflow (reconciler) ──▶ Compute Engine   OpenSSH  ── key-only, no
    ▲     ▲                 get/suspend             │      public ingress
    │     │                                         ▼
    │   guest attribute ◀── VM emitter       workbox VM
    │   (workbox/last_active)                       │
  Cloud Scheduler (1/min)                           ▼
                                             Herdr server + panes
                                                    │
                                             Claude Code sessions
```

- **Control plane** — the Go CLI and a GCP Workflow call Google Cloud APIs
  directly (through the official Go client libraries and Workflows connectors;
  never by shelling out to `gcloud`). They inspect the instance, suspend it
  (only the CLI starts/resumes it), and read/write a small Firestore document.
  Cloud Scheduler triggers the Workflow once per minute.
- **Connectivity plane** — your machine reaches the VM only over Tailscale, then
  ordinary OpenSSH, then Herdr. The VM has a public egress IP for outbound
  package installs and Tailscale coordination, but **no inbound firewall rule**.

The two planes never cross: you can control power while the VM is unreachable
(suspended), and you connect without any cloud-admin credentials.

## Terraform owns infrastructure; the CLI owns runtime state

```
Terraform (infra/)             Go CLI + Workflow (runtime)
─────────────────────          ───────────────────────────
APIs, VPC, subnet              wake / sleep [HH:MM] / status
VM, boot + data disks          keep-awake / cancel / schedule
snapshot policy                reconciler idle-suspend
IAM (3 SAs + operator role)    Firestore hold / scheduled sleep
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
Cloud Scheduler  ──(every minute, OAuth)──▶  Workflow reconciler (suspend-only)
                                                    │
                     gets instance status ◀─────────┤
                     reads hold / scheduled sleep ◀─┼──  Firestore state doc
                     reads last-active ◀────────────┼──  guest attribute (VM emitter)
                     suspends if idle & unprotected ▶  Compute Engine API
```

A GCP **Workflow** is the reconciler rather than an always-on `workboxd` daemon:
there is nothing to keep running, patch, or pay for between ticks. Waking is
always manual (the CLI), so the reconciler **only ever suspends** — it never
resumes or starts. Each run:

1. gets the instance status; does nothing unless it is `RUNNING`;
2. gets the current time in the configured IANA timezone via `time.format`
   (DST-correct, no manual offsets);
3. reads the keep-awake hold and scheduled-sleep spans from Firestore;
4. a scheduled sleep forces a suspend; a keep-awake hold or being within working
   hours inhibits it;
5. otherwise, when idle shutdown is enabled, reads the last-active guest attribute
   the VM emits and suspends once idle ≥ the timeout, counting from the later of
   that value and the instance's `lastStartTimestamp` (a boot grace for every
   start path); a far-future or non-numeric value is ignored, and with neither
   known it fails safe toward suspend;
6. deletes the state document once every span has expired (so cleanup only
   happens while the VM is running).

It is idempotent: an already-suspended VM, or one that should stay up, is left
untouched.

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
optional operational spans. Its field layout is the contract between
`internal/state/firestore.go` and `infra/reconcile.yaml.tftpl`:

```
scheduled_sleep: { start: <timestamp>, end: <timestamp>, state: "asleep" }
hold:            { start: <timestamp>, end: <timestamp>, state: "awake" }   # keep-awake / wake grace
updated_by:      <string>
updated_at:      <timestamp>
```

Each is an optional half-open span `[start, end)`. **Precedence, highest to
lowest:**

```
scheduled sleep (scheduled_sleep, forces suspend even in working hours)
    > keep-awake hold
    > working hours
    > idle shutdown
```

Spans expire when `now` passes their `end`; the reconciler deletes the document
once all present spans have expired (on a tick where the VM is running). The
last-active signal is **not** in this document — it travels as a guest
attribute (`workbox/last_active`) the VM emits, keeping the VM's least-privilege
SA out of Firestore. See the engine in `internal/schedule/schedule.go` and the
day-to-day behavior in [operations.md](operations.md#wake-manually-sleep-on-idle).

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
