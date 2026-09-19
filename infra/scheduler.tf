# Cloud Scheduler triggers the reconciler by POSTing to the Workflows executions
# API. The executions endpoint is a *.googleapis.com service, so it authenticates
# with an OAuth token (not OIDC) from the least-privilege scheduler SA.

resource "google_cloud_scheduler_job" "reconcile" {
  name             = "${local.name}-reconcile"
  region           = local.services_region
  description      = "Trigger the workbox reconciler"
  schedule         = var.reconcile_interval
  time_zone        = local.timezone
  attempt_deadline = "320s"

  retry_config {
    retry_count = 1
  }

  http_target {
    http_method = "POST"
    uri         = "https://workflowexecutions.googleapis.com/v1/${google_workflows_workflow.reconcile.id}/executions"

    oauth_token {
      service_account_email = google_service_account.scheduler.email
    }
  }

  depends_on = [
    google_project_service.apis,
    google_project_iam_member.scheduler_invoker,
  ]
}
