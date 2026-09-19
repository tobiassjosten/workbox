# Remote Terraform state in a GCS bucket.
#
# This is a PARTIAL backend configuration: the bucket name is environment
# specific and cannot be read from the shared YAML (Terraform resolves the
# backend before locals/variables exist), so it is supplied at init time via
#   terraform init -backend-config=backend.hcl
# `make tf-init` does this for you. Copy backend.example.hcl to backend.hcl
# (git-ignored) and fill in your bucket.
#
# The bucket must already exist before the first `terraform init`; Terraform
# cannot bootstrap the bucket that holds its own state. See docs/operations.md
# ("Remote state") for the one-time creation command (versioning + uniform
# access + public-access prevention).
#
# `terraform validate` and CI use `init -backend=false`, which ignores this
# block entirely, so no bucket is needed to validate the configuration.

terraform {
  backend "gcs" {}
}
