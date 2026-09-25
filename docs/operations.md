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
workbox status --json   # machine-readable form (fields in README, "workbox status --json")
```

`wake` is idempotent: it does nothing if already `RUNNING`, resumes if
`SUSPENDED`, starts if `TERMINATED`, and waits out transitional states. `sleep`
skips the suspend if already `SUSPENDED`/`TERMINATED` but still clears the
keep-awake hold and scheduled sleep.

## Wake manually, sleep on idle

The machine is **always woken by hand** — `workbox` or `workbox wake`. It then
**suspends itself** once nothing is happening. The reconciler runs every minute
and only ever *suspends* a running VM; it never wakes one. Two independent,
optional controls decide when it may suspend:

1. **Working hours** — an optional recurring window (e.g. `06:00–23:00` in your
   timezone) during which automatic suspend is disabled. Inside it the VM stays
   up regardless of activity. Working hours never wake the VM; they only hold off
   sleep.
2. **Idle shutdown** — outside working hours (or when no window is configured),
   the VM suspends after `idle_timeout_minutes` (default 30) of **inactivity**.
   Activity means either a herdr agent in the `working` state or any open
   inbound SSH connection — including a `workbox forward` tunnel and an attached
   Herdr client; a `blocked`/`idle`/`done` agent does not count. Close those to
   let the VM sleep. A lingering OpenSSH `ControlPersist` master is an open
   connection too and holds the VM awake until it closes; keep `ControlPersist`
   short for the workbox host if you use multiplexing. Workbox's own probes
   never open one. Long-running commands that are not a Herdr agent (a build in
   a pane, `nohup`, detached containers) do **not** count: run
   `workbox keep-awake 2h` before walking away from one. Idle time counts from
   the later of the last activity and the instance's last start
   (`lastStartTimestamp`), so a freshly started VM — first provisioning, a
   Terraform replacement, a Console start — always gets the full timeout.

Both are set in `schedule` in `~/.config/workbox/config.yaml` (see
`workbox.example.yaml`). Set `idle_timeout_minutes: 0` to disable idle shutdown;
omit `working_hours` (or set `enabled: false`) to apply idle shutdown around the
clock.

### Grace, holds and one-off sleep

```bash
workbox wake             # resume now; a short keep-awake grace covers startup
workbox sleep            # suspend now (alias: workbox off); clears hold + scheduled sleep
workbox sleep 20:00      # one-off: suspend tonight at 20:00 (also accepts 2000)
workbox keep-awake 3h    # wake if needed, then stay awake for a duration
workbox cancel           # clear the scheduled sleep and keep-awake hold
workbox schedule         # working hours, idle setting, hold and scheduled sleep
```

- `workbox wake` sets a keep-awake **grace** of the idle timeout plus the SSH
  wait timeout (`ssh.wait_timeout_seconds`), so a freshly woken VM isn't
  suspended while it resumes or before the activity emitter first reports. With
  idle shutdown disabled there is no grace: it only guards against idle
  shutdown, and a scheduled sleep wins over a hold regardless.
  The startup script also reports activity when it starts, once a minute for up
  to an hour while it runs, and when it finishes.
  A wake that had to wait for zone capacity keeps the hold alive while it waits
  and re-measures it once the VM is up, so the wait is not taken out of it;
  `workbox keep-awake` does the same with the duration you asked for. A wake that
  waited also re-checks the scheduled sleep afterwards, since one in effect would
  otherwise suspend the VM you were just told was awake — see the
  `workbox sleep HH:MM` bullet below, and [Zone has no capacity for the machine
  type](#zone-has-no-capacity-for-the-machine-type).
- `workbox sleep 20:00` schedules a one-off suspend at the next 20:00; it fires
  **even within working hours**. Run `workbox cancel` before then to call it off
  (this also clears any keep-awake hold), or just `workbox wake` afterwards — a
  manual wake cancels a scheduled sleep that is currently in effect. It expires
  at the next working-hours start — which can be days away when
  `working_hours.days` is restricted, so a Friday 20:00 sleep covers the weekend
  — or after 8 h when working hours are absent or disabled.
- `workbox sleep HH:MM` leaves a keep-awake hold in place; where the two overlap,
  the scheduled sleep wins (see precedence below). `workbox keep-awake` cancels
  a scheduled sleep only when the new hold runs past its start — and after a
  capacity wait it re-applies that rule to the hold re-measured from the
  post-wake clock, so a sleep that hold now runs past is cancelled too, as is
  one that came into effect while it waited.
- `workbox keep-awake` also **resumes a suspended VM**, so arming a hold for a
  later session starts the machine (and its billing) now.

**Precedence:** `scheduled sleep > keep-awake hold > working hours > idle
shutdown`. A manual `workbox sleep` always suspends now, and — because the
reconciler never auto-wakes — it stays asleep until you wake it again.

### Changing working hours

Working hours live in config and are baked into the reconciler at apply time:

1. Edit `schedule.working_hours` / `schedule.idle_timeout_minutes` /
   `schedule.timezone` in `~/.config/workbox/config.yaml`.
2. `make tf-apply` — Terraform re-renders the reconciler Workflow.

## Applying provisioning changes

The VM's startup-script (activity emitter, swapfile, sshd settings, tooling) is
Terraform-managed, but GCE runs it only at boot — not on `terraform apply` and not
on suspend/resume. After an apply that changes it, boot the VM once so it takes
effect: `sudo reboot` on the VM is the cheapest, or stop the instance (Console or
`gcloud compute instances stop`) and `workbox wake`. Alternatively, run
`sudo google_metadata_script_runner startup` on the VM without rebooting.

`workbox doctor` warns when the emitter's timer is not active.

### Upgrading from schedule.wake/sleep

The CLI and the reconciler deploy separately, and a running VM predates the
activity emitter, so upgrade in this order:

1. Migrate the config: replace `schedule.wake`/`schedule.sleep` with
   `schedule.working_hours.start`/`end` (both the CLI and Terraform refuse the
   old keys).
2. `workbox keep-awake 1h`. Until the emitter runs, the new reconciler sees no
   activity, and a VM started long ago has no boot grace left, so without a hold
   it is suspended outside working hours on the first tick after the apply —
   even during your SSH session. This save also replaces the old state document,
   dropping its legacy fields.
3. `make tf-apply` right away: until then the old reconciler keeps running its
   baseline schedule and ignores the new CLI's scheduled sleeps.
4. `sudo reboot` on the VM, so the startup script installs the activity emitter.
5. `workbox doctor` to confirm the activity emitter and signal; `workbox cancel`
   if you want to drop the hold early.

Scripts reading `workbox status --json` need updating too: `hold` is now
`keep_awake`; `desired`, `next_transition`, `next_wake`, `next_sleep`,
`wake_override` and `sleep_override` are gone; and
`schedule.wake`/`schedule.sleep` are replaced by `schedule.working_hours`
and `schedule.idle_timeout_minutes`. The new `auto_suspend`, `activity` and
`scheduled_sleep` fields carry the verdict and state instead (see
"workbox status --json" in the README).

## Diagnostics

```bash
workbox doctor
```

Read-only. It checks: config parses; GCP ADC works; the instance is readable;
local `tailscale`, `ssh` and `herdr` exist; and — only if the VM is already
`RUNNING` — SSH reachability, the presence of remote `herdr`, `claude` and the
Herdr/Claude integration hook, that the activity emitter's timer is active and
its last run succeeded, and that a last-active value is actually readable. By
default it never wakes a sleeping VM; pass `--wake` to first wake the VM so the
remote checks can run. Failures print a remediation hint.

### Zone has no capacity for the machine type

```
workbox: europe-north2-a has no capacity for e2-custom-8-16384 right now (ZONE_RESOURCE_POOL_EXHAUSTED)
```

Waking needs Compute Engine to place that exact machine shape in that zone at
that moment: a suspended VM reserves no capacity, and neither does a terminated
one, so both a resume and a cold start can fail even though nothing about your
setup changed. It is a transient shortage, most common in small regions, and
every command that wakes (`workbox`, `wake`, `ssh`, `herdr`, `forward`,
`keep-awake`, `doctor --wake`) already retries for about 5 minutes before
giving up. A failed wake leaves your data alone: the disks and, unless the VM has
since been terminated, the suspended memory state are intact.

Its Firestore state changes do stand, though. The hold it wrote before resuming
— `wake`'s grace, or the duration `keep-awake` was given — was refreshed while
the wake waited, unless the hold already ran well past the retry window, so it
can end somewhat later than the end the command printed. A scheduled sleep the
wake cancelled before waiting stays cancelled: for `wake` that is one already in
effect, for `keep-awake` any the requested hold reaches over. `workbox schedule`
shows where both stand. (The post-wait re-checks under [Grace, holds and one-off
sleep](#grace-holds-and-one-off-sleep) apply only to a wake that succeeded.)

Workbox writes that document whole rather than field by field, and the post-wait
top-up re-reads it first, so only a schedule change from another shell — `sleep`,
`cancel`, or any of the wake commands listed above — that lands between that read
and the write is lost: a brief window, one read-modify-write round trip. When the
hold is short enough that it could lapse mid-wake (a short idle timeout, or a
short `keep-awake` duration) it is also refreshed between attempts, which adds one
such moment per attempt. If you changed the schedule from another shell while a
wake was waiting, `workbox schedule` shows what is on record.

When the retries run out:

- **Try again in a while.** Shortages usually clear in minutes to hours, and
  retrying costs nothing.
- **Change the machine shape.** Another series draws on a different capacity
  pool: set `gcp.machine_type` in your config and run `make tf-apply`. Terraform
  stops the instance to change its shape (`allow_stopping_for_update`), so the
  **suspended memory state is discarded** — the disks are untouched. Keep
  suspend/resume support in mind: no GPUs, no Local SSD, at most 208 GB of
  memory.
- **Move to another zone.** Not just a config change, and not covered by the
  procedures below: a zonal disk only attaches to an instance in its own zone,
  so the detach/attach in [Restoring from a
  snapshot](#restoring-from-a-snapshot) and the in-place `-replace` in
  [Replacing the VM while keeping `/work`](#replacing-the-vm-while-keeping-work)
  are both same-zone operations. A move means migrating the disk and the
  instance by hand and reconciling Terraform state — `google_compute_disk.data`
  carries `prevent_destroy`, and the instance's zone forces replacement, so the
  suspended memory state is lost either way. There is no documented procedure;
  changing `gcp.zone` alone fails the apply.
- **Reservations** guarantee capacity, but you pay for the reserved shape around
  the clock, which cancels out suspend-on-idle. Rarely worth it for a single
  development VM.

When the give-up report names other zones (`GCP reports capacity in: ...`),
those are the zones Compute Engine says can take the shape right now — useful
input for the zone move, not something workbox acts on.

If `workbox status` reports `TERMINATED` instead of `SUSPENDED` after the failed
wake, the memory image is already gone and the next wake cold-boots it, as after
a very long suspension (see [Maximum suspend
duration](#maximum-suspend-duration)) — that boot needs capacity for the same
shape, which is why the retry covers it too.

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
  boot) rather than `resume` it — `wake` handles both paths automatically, so
  nothing breaks, but in-memory Herdr/Claude sessions are gone. The same is true
  after a resume that failed for lack of zone capacity and left the instance
  `TERMINATED` (see [Zone has no capacity for the machine
  type](#zone-has-no-capacity-for-the-machine-type)).
- Treat suspend as an overnight/weekend convenience, not durable storage. Push
  work to Git regularly; the data disk and its snapshots are the durable layer.

## Inspecting operational state

`workbox status` and `workbox schedule` show the keep-awake hold and any
scheduled sleep; `status` also shows the last-active time and the current
auto-suspend verdict. `workbox cancel` clears the hold and scheduled sleep by
deleting the Firestore state document. The reconciler also deletes the document
once every span has expired (on a tick where the VM is running), so state does
not accumulate.

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
