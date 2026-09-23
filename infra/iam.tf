# Three identities, each least-privilege:
#   - the development VM SA:   effectively no cloud authority (logging/metrics only)
#   - the Workflow SA:         inspect + suspend the instance, read activity, r/w state
#   - the Cloud Scheduler SA:  invoke the Workflow, nothing else
# A custom operator role is also defined for the human/CLI identity to self-grant.

# --- Development VM identity -------------------------------------------------
# The VM runs Claude Code, Docker and arbitrary tooling, so it must NOT hold
# ambient admin credentials. It gets a dedicated SA with only telemetry writes.
resource "google_service_account" "vm" {
  account_id   = "${local.name}-vm"
  display_name = "workbox development VM"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "vm_logging" {
  project = local.project
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.vm.email}"
}

resource "google_project_iam_member" "vm_metrics" {
  project = local.project
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.vm.email}"
}

# --- Workflow (reconciler) identity -----------------------------------------
resource "google_service_account" "workflow" {
  account_id   = "${local.name}-workflow"
  display_name = "workbox reconciler workflow"
  depends_on   = [google_project_service.apis]
}

# Exactly the instance verbs the suspend-only reconciler needs, plus operation
# polling. It never wakes the VM, so start/resume are deliberately absent.
resource "google_project_iam_custom_role" "workflow" {
  role_id     = "workboxWorkflow"
  title       = "workbox Workflow reconciler"
  description = "Inspect, read activity and suspend the workbox instance."
  permissions = [
    "compute.instances.get",
    # Read the on-VM activity signal (last-active timestamp) from guest
    # attributes to decide idle-suspend. Read-only; the VM writes it itself.
    "compute.instances.getGuestAttributes",
    "compute.instances.suspend",
    "compute.zoneOperations.get",
  ]
}

resource "google_project_iam_member" "workflow_compute" {
  project = local.project
  role    = google_project_iam_custom_role.workflow.id
  member  = "serviceAccount:${google_service_account.workflow.email}"
}

# Read/write the tiny operational-state document.
resource "google_project_iam_member" "workflow_firestore" {
  project = local.project
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.workflow.email}"
}

# Allow the Workflow to write execution logs.
resource "google_project_iam_member" "workflow_logging" {
  project = local.project
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.workflow.email}"
}

# --- Cloud Scheduler identity -----------------------------------------------
resource "google_service_account" "scheduler" {
  account_id   = "${local.name}-scheduler"
  display_name = "workbox scheduler trigger"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "scheduler_invoker" {
  project = local.project
  role    = "roles/workflows.invoker"
  member  = "serviceAccount:${google_service_account.scheduler.email}"
}

# --- Human/CLI operator role (grant to yourself; see README) -----------------
# Everything `workbox` needs and nothing more: read state + read activity +
# power transitions + read/write the operational-state document.
resource "google_project_iam_custom_role" "operator" {
  role_id     = "workboxOperator"
  title       = "workbox Operator (human/CLI)"
  description = "Run the workbox CLI: inspect, read activity, start/resume/suspend the VM, and edit operational state."
  permissions = [
    "compute.instances.get",
    # Read the last-active guest attribute for `workbox status` and `workbox doctor`.
    "compute.instances.getGuestAttributes",
    "compute.instances.start",
    "compute.instances.resume",
    "compute.instances.suspend",
    "compute.zoneOperations.get",
    "datastore.entities.get",
    "datastore.entities.create",
    "datastore.entities.update",
    "datastore.entities.delete",
    "datastore.databases.get",
  ]
}
