# The reconciler Workflow. Its source is rendered from reconcile.yaml.tftpl with
# the schedule and instance parameters baked in. It runs as the least-privilege
# Workflow SA (instance inspect/suspend + activity read + Firestore r/w only).

resource "google_workflows_workflow" "reconcile" {
  name                = "${local.name}-reconcile"
  region              = local.services_region
  description         = "workbox schedule reconciler"
  service_account     = google_service_account.workflow.id
  deletion_protection = false

  source_contents = templatefile("${path.module}/reconcile.yaml.tftpl", {
    project        = local.project
    zone           = local.zone
    instance       = local.name
    timezone       = local.timezone
    work_start_min = local.work_start_min
    work_end_min   = local.work_end_min
    work_days_map  = jsonencode(local.work_days_map)
    idle_min       = local.idle_min
    activity_key   = local.activity_key
    # The bare key the guest-attributes response is keyed by (the namespace is
    # the queryPath prefix), matched in the template as the Go reader does.
    activity_key_name = local.activity_key_name
    database          = local.firestore_database
    collection        = local.state_collection
    document          = local.state_document
  })

  lifecycle {
    # Mirror internal/schedule.ParseDayTime: both bounds must be HH:MM.
    precondition {
      condition     = !local.work_enabled || local.work_times_ok
      error_message = "schedule.working_hours.start/end must be HH:MM (got \"${local.work_start}\" and \"${local.work_end}\")."
    }
    # Mirror internal/config.Validate: the window is [start, end) in local time,
    # so start must be earlier in the day than end.
    precondition {
      condition     = !local.work_enabled || !local.work_times_ok || local.work_start_min < local.work_end_min
      error_message = "schedule.working_hours.start (${local.work_start}) must be earlier in the day than schedule.working_hours.end (${local.work_end})."
    }
    # Mirror internal/config.Validate (minIdleTimeoutMinutes, which has the full
    # rationale): the emitter's reports can be ~75 s apart and the reconciler
    # ticks each minute, so a shorter (or negative) timeout could suspend a VM
    # that is in use.
    precondition {
      condition     = floor(local.idle_min) == local.idle_min && (local.idle_min == 0 || local.idle_min >= 5)
      error_message = "schedule.idle_timeout_minutes (${local.idle_min}) must be a whole number: 0 (disabled) or at least 5."
    }
    # Mirror internal/config's weekday parsing so an unknown day name fails with
    # a clear message (locals.tf skips unknown names so this is reached).
    precondition {
      condition     = length(local.work_days_unknown) == 0
      error_message = "schedule.working_hours.days: unknown day(s) ${join(", ", local.work_days_unknown)}; use names like mon, tue, ... or monday, tuesday, ..."
    }
    # The Go CLI rejects the retired schedule.wake/sleep keys; fail here too so
    # an unmigrated config can't silently apply an idle-only reconciler.
    precondition {
      condition     = try(local.cfg.schedule.wake, null) == null && try(local.cfg.schedule.sleep, null) == null
      error_message = "schedule.wake/schedule.sleep were replaced by schedule.working_hours.start/end; migrate your config and follow docs/operations.md (\"Upgrading from schedule.wake/sleep\") before applying."
    }
  }

  depends_on = [
    google_project_service.apis,
    google_project_iam_member.workflow_compute,
    google_project_iam_member.workflow_firestore,
  ]
}
