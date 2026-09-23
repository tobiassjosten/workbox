# workbox

Turn a persistent GCP Compute Engine VM into a "second computer." Running `workbox`
wakes the VM, waits for it to become reachable over Tailscale/SSH, and opens a
[Herdr](https://herdr.dev) session. A small control plane keeps the VM asleep on a
schedule so you only pay for compute when you use it.

The VM has a public egress IP but **no public inbound**. All SSH is over Tailscale.

## Architecture

Two independent planes.

```
CONTROL PLANE (power state + schedule)

  workbox CLI ──> GCP Workflow ──> Compute Engine API (suspend / resume / start)
       │                        └─> Firestore state doc (holds, overrides, schedule)
       │
       └─ Cloud Scheduler ──(1/min)──> reconciler Workflow ──> reconciles VM to schedule

CONNECTIVITY PLANE (interactive access)

  local machine ──> Tailscale ──> OpenSSH ──> VM ──> Herdr / Claude Code
                    (MagicDNS)    (key-only)        (server, panes, sessions live on VM)
```

The control plane is a Go CLI plus a GCP Workflow that calls Google Cloud APIs to
change power state and read/write a tiny Firestore state document. The connectivity
plane is entirely local → Tailscale → OpenSSH → VM → Herdr. Because Herdr keeps its
server, panes, and sessions on the VM and the local client merely attaches over
OpenSSH, sessions survive suspend/resume.

See [docs/architecture.md](docs/architecture.md) for detail.

## Prerequisites

- Go 1.24+
- [golangci-lint](https://golangci-lint.run/welcome/install/) v2 (for `make lint`/`make check`):
  `curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.13.2`
- Terraform ≥ 1.5
- gcloud SDK
- OpenSSH
- A Tailscale account and tailnet
- Herdr installed locally (see https://herdr.dev/docs/install/)
- A Claude paid plan (Pro / Max / Team / Enterprise) for Claude Code
- An existing, billing-enabled GCP project

Terraform does **not** create the GCP project, enable billing, or create your
Tailscale account. Those must already exist.

## From empty accounts to a working system

1. **Create your config.** Run `make configure` to create
   `~/.config/workbox/config.yaml` from `workbox.example.yaml` (it never overwrites
   an existing file), then edit it. See [Configuration](#configuration).
2. **Authenticate to Google.** Run `gcloud auth application-default login` (ADC) and
   set the ADC quota project to your GCP project.
3. **Export Tailscale OAuth credentials.** See [Authentication](#authentication).
   Never commit these.

   ```sh
   export TAILSCALE_OAUTH_CLIENT_ID=...
   export TAILSCALE_OAUTH_CLIENT_SECRET=...
   # optional:
   export TAILSCALE_TAILNET=...
   ```

   Or copy `.env.example` to `.env.local` (git-ignored), fill it in, and load it
   with `. ./.env.local`.

4. **Merge the tailnet policy fragment** (required when `manage_policy: false`, the
   default). See [Safe tailnet policy setup](#safe-tailnet-policy-setup). The
   `tagOwners` entry for `tag:workbox` **must** exist before you apply Terraform.
5. **Provision the cloud resources.** Terraform state lives in a GCS bucket, so
   create the bucket and point Terraform at it first (one-time — see
   [docs/operations.md](docs/operations.md#remote-state-gcs-backend) for the full
   commands):

   ```sh
   gcloud storage buckets create gs://YOUR-PROJECT-tf-state \
     --project=YOUR-PROJECT --location=EU \
     --uniform-bucket-level-access --public-access-prevention
   gcloud storage buckets update gs://YOUR-PROJECT-tf-state --versioning
   cp infra/backend.example.hcl infra/backend.hcl   # then edit: set your bucket

   make tf-init
   make tf-apply
   ```

   Note the outputs: `instance_name`, `external_ip`, `tailscale_hostname`,
   `ssh_target`, `data_disk`, `reconciler_workflow`, `operator_role`, `next_steps`.
6. **Grant yourself the operator role** (from the `operator_role` output):

   ```sh
   gcloud projects add-iam-policy-binding PROJECT \
     --member="user:YOUR_EMAIL" \
     --role="projects/PROJECT/roles/workboxOperator"
   ```

7. **Install the CLI.**

   ```sh
   make install
   ```

On first boot the VM's startup script mounts `/work`, joins Tailscale with a
single-use tagged key, hardens SSH to key-only, installs Docker, installs Claude Code
and Herdr for the dev user, and runs `herdr integration install claude`.

## Configuration

Config lives at `~/.config/workbox/config.yaml` by default (honors
`$XDG_CONFIG_HOME`). Override the path with the global `--config PATH` flag or the
`WORKBOX_CONFIG=PATH` environment variable.

The schema, reproduced from `workbox.example.yaml`:

```yaml
name: workbox

gcp:
  project_id: your-project
  region: europe-north2
  zone: europe-north2-a
  machine_type: e2-custom-8-16384   # 8 vCPU / 16 GB
  boot_disk_gb: 30
  data_disk_gb: 200
  # optional:
  # services_region: europe-west3   # defaults to gcp.region; set if region lacks Workflows/Scheduler
  # image_family: ...
  # image_project: ...
  # deletion_protection: true

machine:
  linux_user: developer
  ssh_public_key_file: ~/.ssh/id_ed25519.pub   # ~ expanded; PUBLIC key only
  # optional:
  # data_mount: /work
  # swap_gb: 4                       # swapfile size in GB; 0 disables (default 4)

tailscale:
  hostname: workbox
  tag: tag:workbox
  ssh_target: workbox              # SSH host/alias; the CLI adds -l linux_user
  user: you@example.com            # your tailnet email
  manage_policy: false             # DANGER; see below. Default false.

schedule:
  timezone: Europe/Stockholm
  wake: "06:00"
  sleep: "23:00"                   # wake MUST be earlier than sleep

state:
  firestore_database: "(default)"
  firestore_location: eur3         # europe-north2 is NOT a Firestore location
  collection: workbox
  # document: workbox   # defaults to `name`
```

Notes:

- `ssh_public_key_file` must point at your **public** key.
- `schedule.wake` must be earlier than `schedule.sleep`.
- `state.firestore_location` is `eur3` because `europe-north2` is not a valid
  Firestore location.

## Authentication

### Google

```sh
gcloud auth application-default login
```

Set the ADC quota project to your GCP project.

### Tailscale provider

Create an OAuth client with these scopes:

- **auth_keys (write)**, with your **`tailscale.tag`** attached to the client
  (defaults to `tag:<name>`, e.g. `tag:workbox`)
- **policy-file (write)** — only if `manage_policy: true`

Provide the credentials through environment variables (never commit them):

```sh
export TAILSCALE_OAUTH_CLIENT_ID=...
export TAILSCALE_OAUTH_CLIENT_SECRET=...
export TAILSCALE_TAILNET=...   # optional
```

## Safe tailnet policy setup

The tailnet policy is a **single global file** for your whole tailnet. By default
`manage_policy: false`, and Terraform does **not** touch your policy.

You must first merge the fragment
[`infra/tailscale-policy.example.hujson`](infra/tailscale-policy.example.hujson) into
your existing policy in the Tailscale admin console. It defines `tagOwners` for
`tag:workbox` and a grant giving your identity `TCP:22` to `tag:workbox`.

The `tagOwners` entry **must exist before `terraform apply`**, because the tagged
enrollment key created during apply depends on it.

> **Warning:** Only set `manage_policy: true` on a **dedicated** tailnet. It makes
> Terraform own and **overwrite the entire policy file**, clobbering anything else in
> it.

## First connection, Claude login, Herdr

Add an entry to `~/.ssh/config`:

```
Host workbox
    HostName workbox   # Tailscale MagicDNS name
    User developer
    ForwardAgent yes   # forward your local ssh-agent (private-repo Git on the VM)
```

`ForwardAgent yes` lets Git on the VM authenticate to private repositories using
your local ssh-agent — no key is ever stored on the VM. Omit it if you do not
want agent forwarding. Only interactive sessions forward the agent;
`workbox forward` tunnels never do. The most recent interactive login owns it,
so if Git on the VM loses access, reconnect once.

Connect and finish setup:

```sh
workbox ssh
# on the VM:
claude          # interactive browser/paste login; no credentials are provisioned
herdr --version
```

No Claude credentials are provisioned to the VM — `claude` performs an interactive
login the first time.

Optionally register the machine with Herdr:

```sh
herdr machine add workbox --label "Workbox"
```

Or just use `workbox herdr` (equivalent to `herdr --remote workbox`). Herdr keeps its
server, panes, and sessions on the VM; the local client attaches over OpenSSH, so
sessions survive suspend/resume.

## Reviewing a dev server in your browser

The VM has **no public inbound**, so a dev server (Hugo, Vite, a local web app) is
not reachable directly. `workbox forward` tunnels a local port to the same service
on the VM's loopback over the existing Tailscale/SSH path, so you can open it in a
local browser. Nothing is exposed on the tailnet — the VM side is always
`localhost`, so the tunnel is the only way in.

```sh
# on the VM (e.g. in your Herdr session):
hugo serve                       # binds 127.0.0.1:1313 as usual

# on your local machine:
workbox forward 1313             # then open http://localhost:1313
workbox forward 1313 8080        # forward several ports at once
workbox forward 8080:1313        # map local 8080 -> VM's 1313
```

`forward` checks that each local port is free (so a busy port fails before the VM
is woken), wakes and waits for SSH like `ssh`/`herdr`, then holds the forwards open
with no remote shell (`ssh -N`). Press Ctrl-C to close them. Because the dev server
binds the VM's loopback, no per-project change and no `baseURL`/live-reload tweaks
are needed — the browser sees a genuine `localhost`.

## CLI reference

```
workbox                 # default: wake, wait for SSH, open Herdr
workbox status [--json] # human or JSON state
workbox wake            # resume/start now; sets a stay-awake hold if the schedule wants it asleep
workbox sleep           # (alias: workbox off) suspend now; stay-asleep hold if schedule wants awake
workbox ssh [-- args]   # wake, wait, then hand off to your ssh client
workbox herdr           # wake, wait, open Herdr (same as bare `workbox`)
workbox forward PORT... # wake, wait, then hold local port-forwards to the VM open (browser review)
workbox wake-at HH:MM   # one-workday wake override
workbox sleep-at HH:MM  # one-workday sleep override (handles past-midnight like 01:30)
workbox keep-awake 3h   # hold awake for a Go duration (3h, 90m)
workbox cancel-override # clear overrides and holds
workbox schedule        # show normal schedule, overrides, holds, next transitions
workbox doctor          # read-only diagnostics (never wakes unless you pass --wake)
workbox --version
```

Global flag: `--config PATH`. Environment: `WORKBOX_CONFIG=PATH`.

## Daily usage and schedule overrides

Start your day:

```sh
workbox                    # wake, wait, open Herdr
```

Overrides let you deviate from your configured schedule for a single workday
without editing config.

```sh
workbox sleep-at 01:30     # stay up past midnight tonight; back to normal tomorrow
workbox wake-at 08:30      # sleep in tomorrow
workbox wake-at 05:00      # start early tomorrow
workbox sleep-at 20:00     # wind down early
workbox keep-awake 3h      # hold awake for the next 3 hours
```

Manual actions set holds that the scheduler respects. For example, `workbox wake` at
01:00 will **not** be re-suspended by the reconciler, because `wake` sets a temporary
stay-awake hold.

Inspect current state:

```sh
workbox status
workbox schedule
```

**Precedence:** manual hold > one-workday override > baseline schedule. Overrides are
one-shot and auto-expire.

To change the schedule **permanently**, edit `schedule.wake` / `schedule.sleep` in
your config and run `make tf-apply`. The schedule is not editable at runtime.

## Make targets

```
make configure      # create user config from workbox.example.yaml (never overwrites)
make build
make install        # go install; prints the install directory
make test
make lint
make fmt
make check          # gofmt + golangci-lint + test + build + terraform fmt/validate; no cloud changes
make tf-init        # talks to the cloud
make tf-fmt
make tf-validate
make tf-plan        # talks to the cloud
make tf-apply       # talks to the cloud
```

Terraform targets automatically pass `-var config_file=...` (defaulting to the
canonical config path). `tf-init`, `tf-plan`, and `tf-apply` talk to the cloud; the
others do not.

## Operations

### Rebuilding the VM while keeping the data disk

The data disk is protected with Terraform `prevent_destroy` and `auto_delete=false`,
so it is not deleted when the instance is replaced. To rebuild the VM in place:

1. Set `gcp.deletion_protection: false` in your config and run `make tf-apply`
   (a protected instance cannot be replaced).
2. Remove the old machine in the Tailscale admin console, so the new one can
   register under the same hostname.
3. Replace the instance and the enrollment key:

   ```sh
   terraform -chdir=infra apply -replace=google_compute_instance.workbox \
     -replace=tailscale_tailnet_key.bootstrap \
     -var config_file=$HOME/.config/workbox/config.yaml
   ```

4. Set `gcp.deletion_protection` back to `true` and run `make tf-apply`.

The enrollment key is replaced with the instance because the old one is
single-use and already consumed: a new VM given it could not join the tailnet.


Alternatively, detach and reattach the disk. Either way, **`/work` survives**. See
[docs/operations.md](docs/operations.md) for the exact detach/reattach steps.

### Snapshot restore

Daily snapshots are taken via a resource policy, with **14-day retention**. To
restore, create a new disk from a snapshot and reattach it. See
[docs/operations.md](docs/operations.md).

## Security model

- **No public ingress.** The VM has a public egress IP but accepts no inbound public
  traffic. All SSH is over Tailscale.
- **Key-only SSH.** The startup script hardens SSH to public-key authentication.
- **Least-privilege service accounts.** The VM holds no admin credentials.
- **Single-use Tailscale key.** The tagged enrollment key is single-use; it lives
  in git-ignored Terraform state and in its own instance metadata entry, where it
  is useless once consumed. Terraform never pushes a replacement key to a running
  VM.
- **No secrets in git.** Tailscale OAuth credentials are supplied via environment
  variables; Claude login is interactive on the VM.

See [docs/security.md](docs/security.md).

## Cost

You pay for:

- **The VM** — billed while **RUNNING**. Suspending stops vCPU billing, but you still
  pay for preserved memory and disks.
- **Boot and data disks** — billed continuously.
- **Daily snapshots** — billed per retention policy (14 days).
- **The reconciler** — Cloud Scheduler (3 free jobs), Workflows steps, and Firestore
  reads at roughly once per minute. Approximately **~$3–8/month**; Firestore reads and
  Scheduler generally stay within free tiers.

> **Note:** Compute Engine suspend has a **60-day maximum**. After 60 days the VM
> auto-transitions to `TERMINATED` and in-memory state is lost. `workbox wake` handles
> both cases: it resumes a `SUSPENDED` VM and starts a `TERMINATED` one.

## Troubleshooting

Start with read-only diagnostics (never wakes a sleeping VM by default):

```sh
workbox doctor
```

Then verify:

- Google ADC is set up: `gcloud auth application-default login`
- Tailscale is up: `tailscale status`
- The SSH host resolves via MagicDNS
- The operator role has been granted to your identity
- `herdr` and `claude` are present on the VM
- After upgrading or changing provisioning, the VM was rebooted or cold-restarted
  once so the startup-script (including the activity emitter) ran — see
  [docs/operations.md](docs/operations.md#applying-provisioning-changes); when
  upgrading from `schedule.wake`/`schedule.sleep`, follow
  [the upgrade steps](docs/operations.md#upgrading-from-schedulewakesleep)

## Destroying everything

> **Warning:** `terraform destroy` is intentionally blocked by the data disk's
> `prevent_destroy` and the VM's `deletion_protection`. To truly tear down, you must
> consciously remove those protections first. Doing so **deletes the development disk
> and all unpushed work**. Snapshots persist per the retention policy unless you also
> delete them.

## Documentation

- [docs/architecture.md](docs/architecture.md) — the two planes in detail
- [docs/operations.md](docs/operations.md) — auto-suspend and holds, applying
  provisioning changes, upgrade steps, VM rebuild, disk detach/reattach, snapshot
  restore
- [docs/security.md](docs/security.md) — the full security model
