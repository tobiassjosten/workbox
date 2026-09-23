# Tailscale provisioning.
#
# SAFETY: the tailnet policy file is a single global resource. By default this
# module DOES NOT manage it — you apply the small fragment in
# tailscale-policy.example.hujson by hand so the rest of your policy is
# untouched. Only when tailscale.manage_policy is true (a dedicated tailnet)
# does Terraform own and OVERWRITE the entire policy. See docs/security.md.

locals {
  ts_grant_src = local.ts_user != "" ? local.ts_user : "autogroup:member"

  ts_policy = jsonencode({
    tagOwners = {
      (local.ts_tag) = distinct(compact([local.ts_user, "autogroup:admin"]))
    }
    grants = [
      {
        src = [local.ts_grant_src]
        dst = [local.ts_tag]
        ip  = ["tcp:22"]
      }
    ]
  })
}

# Opt-in: own the ENTIRE tailnet policy. Off by default.
resource "tailscale_acl" "policy" {
  count = local.ts_manage_policy ? 1 : 0
  acl   = local.ts_policy
}

# Single-use, short-lived, pre-authorized, tagged enrollment key. It is consumed
# by the first successful join; a failed join leaves it unconsumed, bounded by
# the 1 h expiry. It is stored sensitively in Terraform state and stays in its
# own instance metadata entry (workbox-ts-authkey) for the life of the instance;
# Terraform ignores later changes to that entry, so a replacement key never
# reaches an already-enrolled VM. Because ignore_changes also suppresses creating
# the entry, instances that predate it have none until they are replaced (see
# docs/operations.md). Keep state out of Git. The node is NOT ephemeral, so it
# keeps its identity across suspend/resume.
resource "tailscale_tailnet_key" "bootstrap" {
  reusable            = false
  ephemeral           = false
  preauthorized       = true
  expiry              = 3600
  description         = "workbox VM bootstrap single-use"
  tags                = [local.ts_tag]
  recreate_if_invalid = "never"

  # When Terraform manages the policy, ensure the tag owner exists first.
  depends_on = [tailscale_acl.policy]
}
