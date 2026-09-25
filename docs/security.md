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

The idle-suspend **activity signal is one-way and needs no extra VM privilege**:
the VM writes its own `workbox/last_active` guest attribute (authorized by the
metadata server for the instance itself, gated by `enable-guest-attributes`), and
the reconciler *reads* it with `compute.instances.getGuestAttributes`. The VM SA
is unchanged — the signal never puts the VM near Firestore or any admin API.
The trade-off: any process on the VM can write that attribute too, so an
untrusted workload could keep reporting activity and hold the VM awake. A single
forged write does not last: the reconciler ignores a value more than five minutes
in the future or one that is not a number, so each forged value expires like a
real one. The impact is cost only — no privilege is gained, and a scheduled sleep
still wins — and idle shutdown is a cost control, not a security boundary.

**Text workbox did not author is bounded and stripped before it is printed.**
Two sources reach the terminal: the VM's `workbox/last_active` guest attribute,
which any process on the VM can write, and the Compute API's capacity error
details (zone, machine type, the alternative zones, Google's own message). Both are
cut to a rune bound in `internal/compute` — the list of alternative zones to a
length bound as well — and neither reaches a terminal raw: the guest attribute is
rendered with `%q`, which escapes anything non-printable, and the capacity text is
normalized, dropping control and format runes (the ESC that would start an escape
sequence and the bidi overrides that would reorder a line among them) and
collapsing whitespace to single spaces, so the single-line error stays one line.
Either way the text is passed as a formatting *argument*, never as a format string.
A terminal cannot be repainted, nor a message forged, by whatever is on the other
end.

The signal is a socket check, not a session check: the emitter counts any
established inbound connection on port 22, and a socket reaches that state
before SSH authentication. Anyone the tailnet policy lets reach `tcp:22` can
therefore hold the VM awake by connecting repeatedly, without any key. The
shipped policy fragment grants a single identity; when Terraform owns the policy
(`tailscale.manage_policy: true`) the grant is `autogroup:member` — every member
of your tailnet — unless `tailscale.user` is set. `LoginGraceTime 30` closes each
unauthenticated connection after 30 s. Again the cost is money, not access: an
unauthenticated peer gets no shell, no data and no privilege.

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
  `KbdInteractiveAuthentication no`, `PermitRootLogin no`, public-key only. It
  also sets `ClientAliveInterval 60` / `ClientAliveCountMax 3`, so a dead session
  (laptop asleep, off the tailnet) is dropped within ~3 min and its stale
  ESTABLISHED socket stops counting as activity.
- `workbox ssh` execs the user's **local** OpenSSH, so hardware-backed keys,
  ssh-agent, `known_hosts`, `ProxyJump` and `ControlMaster` all work as usual
  (a persistent master counts as activity and holds off idle shutdown; the
  non-interactive probes opt out of multiplexing). There is no Go SSH
  implementation and no key material in this repository.

### Forwarded ssh-agent (Git access from the VM)

To use private Git repositories from the VM without storing a key on it, the
bootstrap wires up **ssh-agent forwarding** — never a stored key:

- The user's client opts in (`ForwardAgent yes` for the workbox host in
  `~/.ssh/config`); forwarding is never forced by the CLI.
- A bootstrap-managed `/etc/ssh/sshrc` links the forwarded socket to a stable
  path (`~/.ssh/ssh_auth_sock`) on every login (last-writer-wins), so herdr's
  persistent panes — which inherit a stale environment — reach the agent through
  that path. Repointing on every login is deliberate: after a suspend/resume the
  previous connection's socket can survive as a socket file while being dead from
  the client side, and a "claim only when free" scheme would pin the stable path
  to that defunct agent so every wake would fail with publickey. The trade-off is
  that concurrent logins steal the link from each other — a second login repoints
  the path, so the last to log in owns the agent; keep a single active forwarding
  connection. Shells adopt the path only while it points at a live socket.
- Only interactive logins (`workbox`, `workbox ssh`, `workbox herdr`) claim the
  agent. The CLI's reachability probe and `workbox forward` tunnels pass
  `ForwardAgent=no`, so they never repoint the link to a connection that is
  about to close or that carries no agent.
- **Only a socket symlink lives on the VM — still no key material.** While a
  forwarding connection is open, the agent is reachable by anything running as
  the dev user, which is consistent with the threat model (that identity is
  already assumed to run untrusted workloads and carries no cloud authority).
  The agent becomes unreachable once the connection closes.

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

- The key value is **sensitive**. It sits in Terraform state and in its own
  instance metadata entry (`workbox-ts-authkey`, not the startup-script) for the
  life of the instance; it is consumed by the first successful join and is then
  useless. A failed join is non-fatal, so the key can remain unconsumed — the
  one-hour `expiry` is what bounds it then. The
  startup script reads it only when the VM is not yet enrolled. Terraform ignores
  later changes to that entry, so a replaced key (any change to its arguments,
  or a `-replace`) never lands, live and unused, in a running VM's metadata.
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

Firestore holds only the tiny scheduling document (scheduled-sleep and
keep-awake spans, and timestamps) — **no secrets, no project data**. Access is
by IAM, not public Firestore rules: the Workflow SA and the operator role can
read/write it; nothing else can.

## Least privilege (IAM summary)

Three service accounts plus a human role, each scoped to exactly its job:

| Identity            | Grants                                                                 |
|---------------------|------------------------------------------------------------------------|
| `workbox-vm`        | `logging.logWriter`, `monitoring.metricWriter` (telemetry only)        |
| `workbox-workflow`  | custom role: `compute.instances.get/getGuestAttributes/suspend`, `compute.zoneOperations.get`; plus `roles/datastore.user`, `roles/logging.logWriter` |
| `workbox-scheduler` | `roles/workflows.invoker`                                              |
| human/CLI (you)     | custom `workboxOperator` role: `compute.instances.get/getGuestAttributes/start/resume/suspend`, `compute.zoneOperations.get`; plus Firestore `datastore.entities.get/create/update/delete` and `datastore.databases.get` |

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
