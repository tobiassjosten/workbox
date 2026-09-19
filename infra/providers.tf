# Google auth comes from Application Default Credentials:
#   gcloud auth application-default login
#
# Tailscale auth comes from an OAuth client, supplied via environment variables
# (never committed):
#   export TAILSCALE_OAUTH_CLIENT_ID=...
#   export TAILSCALE_OAUTH_CLIENT_SECRET=...
# The OAuth client needs the auth_keys (write) scope with tag:workbox attached,
# and — only if tailscale.manage_policy is true — the policy-file (write) scope.

provider "google" {
  project = local.project
  region  = local.region
  zone    = local.zone
}

provider "tailscale" {
  # Credentials and tailnet are read from TAILSCALE_OAUTH_CLIENT_ID,
  # TAILSCALE_OAUTH_CLIENT_SECRET and (optionally) TAILSCALE_TAILNET.
}
