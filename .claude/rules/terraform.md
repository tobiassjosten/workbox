---
description: Terraform conventions for workbox
globs: ["infra/**"]
---

# Terraform rules

- Keep `terraform fmt` clean and `terraform validate` passing (`make tf-validate`,
  no cloud access needed). CI runs `fmt -check`, `init -backend=false`, `validate`.
- **Least privilege IAM.** Three identities, each minimal: the VM SA has only
  telemetry roles, the Workflow SA has a custom role with exactly the instance
  verbs plus `datastore.user`, the Scheduler SA has only `workflows.invoker`.
  Never attach primitive roles (Owner/Editor) and never use the default compute
  SA on the VM. Document any non-obvious permission inline.
- **Lifecycle protection.** The development disk has `prevent_destroy`; the VM has
  configurable `deletion_protection`; Firestore uses a protective deletion policy.
  Do not remove these to make an operation convenient.
- **No secrets or state in Git.** No credentials in `.tfvars`; provider auth comes
  from ADC (Google) and environment variables (Tailscale OAuth). State is
  git-ignored; `.terraform.lock.hcl` IS committed. The single-use Tailscale key
  is marked sensitive and never output.
- **Tailnet policy is global.** `tailscale_acl` overwrites the ENTIRE policy, so
  it is created only when `tailscale.manage_policy` is true. Default off; ship a
  fragment for manual merge instead.
- **One config source.** Read values via `yamldecode(file(pathexpand(var.config_file)))`
  in `locals.tf`; mirror `internal/config` defaults there. Expand `~` with
  `pathexpand`.
- Use explicit `depends_on` only where Terraform cannot infer ordering (API
  enablement, tag-owner-before-key). Avoid `null_resource`/`local-exec` and never
  shell out to `gcloud`.
- The reconciler lives in `reconcile.yaml.tftpl`. Its Firestore field layout is a
  contract with `internal/state/firestore.go` — change both together. Switch
  conditions route with `next:` only. Escape shell/Workflows `$` as `$$`.
- Before changing a resource schema or provider argument, verify current official
  Google/Tailscale provider docs.
