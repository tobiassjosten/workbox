# Security model

## Threat model

The workbox VM deliberately runs code that should be treated as untrusted: Claude
Code, its dependencies, arbitrary development tooling, Docker containers and MCP
servers. The design assumes that anything running on the VM may be hostile or
compromised, and limits the blast radius accordingly:

- a compromised VM must not be able to administer the GCP project;
- the machine must have no public attack surface;
- no secret material (private keys, tokens, Terraform state, Claude credentials)
  lives in Git or is provisioned onto the VM;
- the global Tailscale policy is never casually overwritten.

Each point below explains how that is enforced.

## Arbitrary code on the VM

Claude Code and project dependencies execute arbitrary code. Two containment
measures matter most: the VM's **service account has no administrative power**
(see below), and the VM has **no inbound network exposure**. A compromise is
contained to the VM and its data disk; it cannot pivot into project
administration or be reached from the internet.

## Metadata and service-account exposure

Any process on a GCP VM can read the instance metadata server, including the
attached service account's access token. Therefore the VM's identity must be
harmless:

- the VM runs under a **dedicated least-privilege service account**
  (`workbox-vm`) with only `roles/logging.logWriter` and
  `roles/monitoring.metricWriter` — telemetry writes, nothing else;
- it is emphatically **not** the default Compute Engine service account (which
  carries broad Editor by default);
- OAuth scopes are further narrowed to logging/monitoring write.

A token stolen from the metadata server can write logs and metrics and nothing
more.

## Public IP but no public ingress

The VM has an ephemeral external IP for outbound traffic (package installs,
Tailscale coordination). The custom VPC adds **no ingress firewall rule**, so
GCP's implied "deny all ingress" stands. There is no `0.0.0.0/0` SSH rule and no
public port. Inbound access is exclusively over Tailscale.

## SSH keys

- Only the configured **public** key is uploaded, via instance metadata
  (`ssh-keys`), with `block-project-ssh-keys=true`. **No private key ever reaches
  the VM.**
- `sshd` is hardened by the bootstrap script: `PasswordAuthentication no`,
  `KbdInteractiveAuthentication no`, `PermitRootLogin no`, public-key only.
- `workbox ssh` execs the user's **local** OpenSSH, so hardware-backed keys,
  ssh-agent, `known_hosts`, `ProxyJump` and `ControlMaster` all work as usual.
  There is no Go SSH implementation and no key material in this repository.

## Tailscale

- The VM joins the tailnet tagged `tag:workbox`.
- Access is least-privilege: a grant permits the configured identity's devices to
  reach **TCP/22** on `tag:workbox` nodes — ordinary OpenSSH over the tailnet.
  Tailscale SSH is intentionally not used (`--ssh=false`).
- The node is **not ephemeral**, so it keeps its Tailscale identity across
  suspend/resume.

### Tailscale enrollment credentials

Provisioning needs a one-time way for the VM to join the tailnet. The enrollment
key is created by Terraform as:

- **single-use** (`reusable=false`),
- **short-lived** (`expiry=3600`, one hour),
- **pre-authorized** (`preauthorized=true`),
- **tagged** (`tag:workbox`),
- **non-ephemeral** (`ephemeral=false`).

Consequences and the accepted trade-off:

- The key value is **sensitive** and appears briefly in the VM's startup-script
  metadata and in Terraform state. It is consumed on first boot and is then
  useless.
- It is **never** emitted as a Terraform output and never appears in docs.
- There is **no reusable, long-lived auth token** left on the VM.
- Terraform state therefore transiently contains a credential — which is exactly
  why **state must never be committed** (`.gitignore` covers `*.tfstate*`,
  `*.tfplan*`, `.terraform/`). State lives in a private, encrypted GCS backend
  (see below), keeping it off local disk and out of Git.

### Tailnet policy is a global resource

The tailnet policy file is a single shared object for the whole tailnet.
Overwriting it can break unrelated devices. Therefore:

- by default (`tailscale.manage_policy = false`) Terraform **does not touch** the
  policy; you merge the fragment in `infra/tailscale-policy.example.hujson` into
  your existing policy by hand;
- only on a **dedicated tailnet**, with `manage_policy = true` consciously set,
  does Terraform own and overwrite the whole policy via `tailscale_acl`.

## Terraform state

State can contain sensitive values (notably the Tailscale key), so it is stored
in a **private, versioned GCS backend** (encryption at rest, no state on local
disk, applyable from more than one machine) and is git-ignored as defense in
depth. The backend bucket must use uniform bucket-level access and enforced
public-access prevention — never expose it. The per-environment bucket name
lives in git-ignored `infra/backend.hcl` (not in committed config). Setup and
the one-time bucket-creation commands are in
[operations.md](operations.md#remote-state-gcs-backend). Never commit state or
plan files.

## Firestore operational state

Firestore holds only the tiny scheduling document (override/hold spans and
timestamps) — **no secrets, no project data**. Access is by IAM, not public
Firestore rules: the Workflow SA and the operator role can read/write it; nothing
else can.

## Least privilege (IAM summary)

Three service accounts plus a human role, each scoped to exactly its job:

| Identity            | Grants                                                                 |
|---------------------|------------------------------------------------------------------------|
| `workbox-vm`        | `logging.logWriter`, `monitoring.metricWriter` (telemetry only)        |
| `workbox-workflow`  | custom role: `compute.instances.get/start/resume/suspend`, `compute.zoneOperations.get`; plus `roles/datastore.user`, `roles/logging.logWriter` |
| `workbox-scheduler` | `roles/workflows.invoker`                                              |
| human/CLI (you)     | custom `workboxOperator` role: `compute.instances.get/start/resume/suspend`, `compute.zoneOperations.get`; plus Firestore `datastore.entities.get/create/update/delete` and `datastore.databases.get` |

No primitive roles (Owner/Editor) are used anywhere. Grant yourself the operator
role with the command in the `next_steps` / `operator_role` Terraform outputs.

## Claude authentication

Claude Code is installed during provisioning, but **no Claude credentials are
provisioned**. You log in interactively by running `claude` on the VM (browser or
paste-code flow); credentials land in `~/.claude/.credentials.json` on the VM
only. No project-wide `ANTHROPIC_API_KEY` is configured.

## Backups and recoverability

Unpushed work has a recovery path independent of any single VM:

- the development disk has Terraform `prevent_destroy`;
- a daily snapshot policy (14-day retention) captures it;
- snapshots are retained even if the source disk is deleted
  (`on_source_disk_delete = KEEP_AUTO_SNAPSHOTS`).

When editing disk or snapshot resources, preserve both protections. Restore steps
are in [operations.md](operations.md).

## If a change would weaken any of this

Widening IAM, opening ingress, weakening `sshd`, leaving a reusable Tailscale
token, or taking ownership of a shared tailnet policy are all regressions of this
model. Prefer the more restrictive option and surface the trade-off rather than
quietly relaxing a control.
