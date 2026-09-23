---
description: workbox threat model and security rules
globs: ["**"]
---

# Security rules & threat model

Workbox runs untrusted-by-assumption workloads (Claude Code, arbitrary
dependencies, Docker containers, MCP servers) on a VM reachable only over a
private tailnet. Design to that model.

## Non-negotiables

- **The development VM gets no ambient cloud admin.** It runs under a dedicated
  least-privilege service account (telemetry writes only). Anything running on
  the VM can read instance metadata and the VM's SA token, so that identity must
  never carry project-admin power. Do not add roles to the VM SA for convenience.
- **No public ingress.** There is a public egress IP but no inbound firewall rule.
  SSH is key-only (`PasswordAuthentication no`, `PermitRootLogin no`) and reached
  over Tailscale. Never add a `0.0.0.0/0` SSH rule.
- **No private keys on the VM.** Only the configured SSH *public* key is uploaded
  via metadata. `workbox ssh` uses the user's local OpenSSH (agent, hardware keys,
  known_hosts) — there is no Go SSH stack and no key material in the repo.
- **Secrets never enter Git.** No Terraform state, `*.tfvars`, `.env`, credentials,
  auth tokens, or Claude credentials. `.gitignore` enforces this; keep it strict.
- **Tailscale enrollment key:** single-use, short-lived (1h), pre-authorized,
  tagged, `ephemeral=false`. It stays in its own instance metadata entry
  (`ignore_changes`, so a replaced key never reaches a running VM) and in
  (git-ignored) Terraform state; it is useless once consumed. Never leave a
  reusable long-lived auth token on the VM. Never print it or put it in an output.
- **Tailnet policy is a global resource.** Never overwrite a user's whole policy.
  Terraform owns it only behind the explicit `tailscale.manage_policy` opt-in.
- **Firestore holds only tiny operational state**, no secrets, guarded by IAM
  (not public rules).
- **Claude authentication is never provisioned.** The CLI is installed; the human
  logs in interactively later. No project-wide API key on the VM.

## Recoverability

Unpushed work must have a recovery path: the development disk has `prevent_destroy`
and a daily snapshot policy. When changing disk/snapshot resources, preserve both.

## When in doubt

Prefer the more restrictive option and document the trade-off. If a change would
widen IAM, expose ingress, weaken SSH, or broaden tailnet policy, stop and surface
it rather than proceeding.
