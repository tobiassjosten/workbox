# Operations

Day-to-day control of the workbox. All commands here are API calls or read-only
diagnostics; none of them run Terraform (except where explicitly changing
baseline infrastructure).

## Waking and suspending

```bash
workbox            # wake, wait for SSH, open Herdr (the usual morning command)
workbox wake       # resume/start now
workbox sleep      # suspend now  (alias: workbox off)
workbox status     # human-readable VM + schedule state
workbox status --json   # stable machine-readable form
```

Both `wake` and `sleep` are idempotent: `wake` does nothing if already `RUNNING`,
resumes if `SUSPENDED`, starts if `TERMINATED`, and waits out transitional states;
`sleep` does nothing if already `SUSPENDED`/`TERMINATED`.

### Manual actions do not fight the scheduler

The reconciler runs every minute. To stop it undoing a manual action, `wake`,
`sleep` and `keep-awake` set a temporary **hold**:

- `workbox wake` at 01:00 (baseline says asleep) → resumes **and** holds awake
  until the next scheduled sleep, so the reconciler won't re-suspend it.
- `workbox sleep` at 22:00 (baseline says awake until 23:00) → suspends **and**
  holds asleep until the next scheduled wake.
- When the baseline already agrees (e.g. `wake` at 10:00), no hold is created.

Holds are shown in `workbox status` and expire on their own.

## Schedule: temporary overrides

Normal schedule is `06:00–23:00 Europe/Stockholm`. Override a single workday
without changing the baseline:

```bash
workbox sleep-at 01:30   # stay up past midnight tonight; back to normal tomorrow
workbox sleep-at 20:00   # sleep early tonight only
workbox wake-at 08:30    # sleep in tomorrow (next wake delayed to 08:30)
workbox wake-at 05:00    # wake early for the next workday
workbox keep-awake 3h    # keep awake for at least three hours (Go duration)
workbox cancel-override  # clear all overrides and holds; back to normal
workbox schedule         # normal schedule, active override/hold, next transitions
```

Each override resolves `HH:MM` to an absolute timestamp on the nearest relevant
day — the command prints the resolved time and timezone so there is no ambiguity
(e.g. `sleep-at 01:30` in the evening resolves to **tomorrow** 01:30). Overrides
are one-shot and expire automatically; future days return to the baseline.

**Precedence:** `manual hold > one-workday override > baseline`.

### Worked timeline

Suppose it is 21:00 and you run `workbox sleep-at 01:30`:

```
20:00 ────────── 23:00 ───────── 01:30 ───────── 06:00
       baseline    │  override:    │  baseline      │  baseline
        awake      │  stay AWAKE   │   asleep       │   wake
                   └─ normal sleep suppressed ──┘
```

The reconciler keeps the VM awake through 01:30, then suspends it; 06:00 the next
morning wakes as usual. The following night's 23:00 sleep is unaffected.

## Schedule: permanent changes

Editing the recurring 06:00/23:00 at runtime would create drift. To change it
permanently:

1. Edit `schedule.wake` / `schedule.sleep` (or `schedule.timezone`) in
   `~/.config/workbox/config.yaml`.
2. `make tf-apply` — Terraform re-renders the reconciler Workflow with the new
   baseline.

## Diagnostics

```bash
workbox doctor
```

Read-only. It checks: config parses; GCP ADC works; the instance is readable;
local `tailscale`, `ssh` and `herdr` exist; and — only if the VM is already
`RUNNING` — SSH reachability and the presence of remote `herdr`, `claude` and the
Herdr/Claude integration hook. By default it never wakes a sleeping VM; pass `--wake` to first wake the
VM so the remote SSH/Herdr/Claude checks can run. Failures print a
remediation hint.

### Tailscale / SSH not working

```bash
tailscale status                 # locally: is your machine on the tailnet?
ssh -v workbox                   # verbose SSH; does the MagicDNS name resolve?
```

Check, in order: your machine is connected to the tailnet; the `tag:workbox`
`tagOwners` and the `tcp:22` grant exist in your tailnet policy; the VM shows up
in the Tailscale admin console; you granted yourself the `workboxOperator` IAM
role; and `~/.ssh/config` has a `Host workbox` entry (or use the full MagicDNS
name).

The enrollment key's metadata entry (`workbox-ts-authkey`) is created with the
instance, and `ignore_changes` also stops Terraform adding it to one that
already exists — so a VM that predates it has no such entry until it is
replaced. That only matters if the VM loses its tailnet identity, in which case
the recovery below applies.

If the VM never appears in the admin console, it failed to join during
provisioning (the join is non-fatal, so the rest of the bootstrap still ran).
Read the instance's serial-console output for `[workbox-bootstrap] tailnet join
failed` or `no enrollment key in metadata`; if either is there, put a fresh
single-use tagged key in the instance's `workbox-ts-authkey` metadata entry and
reboot the VM — the startup script retries the join on the next boot:

```bash
gcloud compute instances add-metadata workbox --zone=ZONE \
  --metadata workbox-ts-authkey=tskey-auth-...
gcloud compute instances stop workbox --zone=ZONE   # clean stop, not a reset
workbox wake                                        # start; re-runs the script
```

Terraform's `ignore_changes` leaves the hand-set value alone afterwards.

## Replacing the VM while keeping `/work`

The data disk has `prevent_destroy` and `auto_delete = false`, so it is never
deleted with the instance. `fstab` mounts it by UUID, so `/work` returns
automatically on the new VM.

**Option A — Terraform-driven (recommended):**

First set `gcp.deletion_protection: false` in your config and `make tf-apply`:
Compute Engine refuses to delete a protected instance, so the replace would
fail. Then remove the old machine in the Tailscale admin console, so the new VM
registers under the same hostname (and `ssh_target` keeps resolving); a name
still taken at enrollment gets a numeric suffix. Then:

```bash
terraform -chdir=infra apply -replace=google_compute_instance.workbox \
  -replace=tailscale_tailnet_key.bootstrap \
  -var config_file=$HOME/.config/workbox/config.yaml
```

Afterwards set `gcp.deletion_protection` back to `true` and `make tf-apply`.

Terraform destroys and recreates the instance; the data disk is untouched and
re-attached with the same stable device name. The enrollment key is replaced
with it because the old one is single-use and already consumed: a new VM given
it would fail `tailscale up` and never join the tailnet.

**Option B — manual detach/reattach:**

```bash
gcloud compute instances detach-disk workbox \
  --disk=workbox-data --zone=ZONE
# ... recreate or repair the instance ...
gcloud compute instances attach-disk workbox \
  --disk=workbox-data --device-name=workbox-data --zone=ZONE
```

The `--device-name=workbox-data` is essential: it yields
`/dev/disk/by-id/google-workbox-data`, matching what the bootstrap mounts.

## Restoring from a snapshot

Daily snapshots (14-day retention) are created by the resource policy.

```bash
# 1. Find a snapshot.
gcloud compute snapshots list --filter="name~workbox-data"

# 2. Create a new disk from it.
gcloud compute disks create workbox-data-restored \
  --source-snapshot=SNAPSHOT_NAME \
  --zone=ZONE --type=pd-balanced

# 3. Suspend/stop the VM, detach the current data disk, attach the restored one
#    with the SAME device name so /work mounts unchanged.
workbox sleep
gcloud compute instances detach-disk workbox --disk=workbox-data --zone=ZONE
gcloud compute instances attach-disk workbox \
  --disk=workbox-data-restored --device-name=workbox-data --zone=ZONE
workbox wake
```

If you want the restored disk to become the canonical one that Terraform manages,
reconcile names in `infra/storage.tf` deliberately (and mind `prevent_destroy`).

## Maximum suspend duration

Compute Engine suspends for at most **60 days**. Past that, the instance
auto-transitions to `TERMINATED` and the **preserved memory/process state is
lost** (the disks persist). Practical implications:

- After a very long suspension, `workbox wake` will `start` the instance (a fresh
  boot) rather than `resume` it — the reconciler and CLI handle both paths
  automatically, so nothing breaks, but in-memory Herdr/Claude sessions are gone.
- Treat suspend as an overnight/weekend convenience, not durable storage. Push
  work to Git regularly; the data disk and its snapshots are the durable layer.

## Inspecting operational state

`workbox status` and `workbox schedule` show the active override and hold.
`workbox cancel-override` clears them by deleting the Firestore state document.
The reconciler also deletes the document once every span has expired, so state
does not accumulate.

## Remote state (GCS backend)

Terraform stores its state in a GCS bucket rather than a local file. This gives
encryption at rest, object-versioned history, and state you can apply from more
than one machine. State still holds sensitive values (including the single-use
Tailscale enrollment key) — the bucket is private and IAM-guarded; never make it
public and never commit state.

The bucket must exist **before** the first `terraform init`; Terraform cannot
bootstrap the bucket that holds its own state. One-time setup:

```bash
# 1. Create a private, versioned bucket (pick a globally-unique name; a common
#    convention is "<project-id>-tf-state"). Match the workbox location.
gcloud storage buckets create gs://YOUR-PROJECT-tf-state \
  --project=YOUR-PROJECT \
  --location=EU \
  --uniform-bucket-level-access \
  --public-access-prevention

# 2. Enable object versioning so a bad apply can be rolled back.
gcloud storage buckets update gs://YOUR-PROJECT-tf-state --versioning

# 3. Point Terraform at it (backend.hcl is git-ignored).
cp infra/backend.example.hcl infra/backend.hcl
#   edit infra/backend.hcl: set bucket = "YOUR-PROJECT-tf-state"

# 4. Initialize against the remote backend.
make tf-init
```

`make tf-init` reads `infra/backend.hcl` and refuses to run without it. The
bucket name is deliberately not in the YAML config or any committed file: a
Terraform backend is resolved before variables/locals exist, so it cannot read
the shared config — `backend.hcl` is the one small exception to "one config
source", kept out of Git alongside `*.tfvars`.

`terraform validate` and CI use `init -backend=false` and need no bucket.

If you ever have local state to migrate, `terraform -chdir=infra init
-migrate-state -backend-config=backend.hcl` moves it into the bucket.

## Tearing down

`terraform destroy` is intentionally obstructed:

- the development disk's `prevent_destroy` will **block** the destroy;
- the VM's `deletion_protection` (when enabled in config) blocks instance
  deletion.

To truly tear everything down you must consciously remove those protections
(`prevent_destroy` in `infra/storage.tf`, and set `gcp.deletion_protection:
false` then `make tf-apply`). **Doing so deletes the development disk and any
unpushed work.** Snapshots created by the policy persist independently until they
age out or you delete them explicitly with `gcloud compute snapshots delete`.
