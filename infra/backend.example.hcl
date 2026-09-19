# Terraform GCS backend configuration — TEMPLATE.
#
# Copy to backend.hcl (git-ignored) and set your values:
#   cp backend.example.hcl backend.hcl
#
# `make tf-init` passes this via `-backend-config=backend.hcl`.
# The bucket must exist before the first init — see docs/operations.md.

# Globally-unique GCS bucket that holds Terraform state. A common convention is
# "<project-id>-tf-state". Keep it in the same location as the rest of workbox.
bucket = "REPLACE-with-your-tf-state-bucket"

# Object prefix within the bucket. State is written to gs://<bucket>/<prefix>/.
prefix = "workbox/state"
