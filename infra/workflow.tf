# The reconciler Workflow. Its source is rendered from reconcile.yaml.tftpl with
# the schedule and instance parameters baked in. It runs as the least-privilege
# Workflow SA (instance start/resume/suspend + Firestore r/w only).

resource "google_workflows_workflow" "reconcile" {
  name                = "${local.name}-reconcile"
  region              = local.services_region
  description         = "workbox schedule reconciler"
  service_account     = google_service_account.workflow.id
  deletion_protection = false

  source_contents = templatefile("${path.module}/reconcile.yaml.tftpl", {
    project    = local.project
    zone       = local.zone
    instance   = local.name
    timezone   = local.timezone
    wake_min   = local.wake_min
    sleep_min  = local.sleep_min
    database   = local.firestore_database
    collection = local.state_collection
    document   = local.state_document
  })

  lifecycle {
    # Mirror internal/config.Validate: the baseline is awake during [wake, sleep)
    # in local time, so wake must be earlier in the day than sleep. Without this a
    # hand-edited config with wake >= sleep would apply a reconciler whose awake
    # window is empty and the VM would never wake on baseline.
    precondition {
      condition     = local.wake_min < local.sleep_min
      error_message = "schedule.wake (${local.wake}) must be earlier in the day than schedule.sleep (${local.sleep})."
    }
  }

  depends_on = [
    google_project_service.apis,
    google_project_iam_member.workflow_compute,
    google_project_iam_member.workflow_firestore,
  ]
}
