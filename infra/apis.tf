# Enable exactly the APIs the implementation uses. Other resources depend on
# these via depends_on so Terraform enables them first.

locals {
  required_apis = [
    "cloudresourcemanager.googleapis.com", # manage project services/IAM
    "compute.googleapis.com",              # the VM, disks, snapshots, network
    "iam.googleapis.com",                  # service accounts and custom roles
    "firestore.googleapis.com",            # operational state
    "workflows.googleapis.com",            # the reconciler
    "workflowexecutions.googleapis.com",   # Scheduler -> Workflows executions
    "cloudscheduler.googleapis.com",       # the per-minute trigger
  ]
}

resource "google_project_service" "apis" {
  for_each = toset(local.required_apis)

  project = local.project
  service = each.value

  # Keep APIs enabled if the config is destroyed; other projects may rely on them.
  disable_on_destroy         = false
  disable_dependent_services = false
}
