# The persistent development disk lives separately from the boot disk so it
# survives VM replacement and reprovisioning. It is protected two ways:
#   - prevent_destroy stops Terraform from ever deleting it
#   - a daily snapshot policy provides a recovery path for unpushed work
# See docs/operations.md for detach/reattach and snapshot restore.

resource "google_compute_disk" "data" {
  name = "${local.name}-data"
  type = "pd-balanced"
  zone = local.zone
  size = local.data_disk_gb

  labels     = local.labels
  depends_on = [google_project_service.apis]

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_compute_resource_policy" "snapshots" {
  name   = "${local.name}-daily-snapshots"
  region = local.region

  snapshot_schedule_policy {
    schedule {
      daily_schedule {
        days_in_cycle = 1
        start_time    = var.snapshot_time_utc
      }
    }
    retention_policy {
      max_retention_days    = var.snapshot_retention_days
      on_source_disk_delete = "KEEP_AUTO_SNAPSHOTS"
    }
    snapshot_properties {
      labels            = local.labels
      storage_locations = [local.region]
    }
  }

  depends_on = [google_project_service.apis]
}

resource "google_compute_disk_resource_policy_attachment" "data" {
  name = google_compute_resource_policy.snapshots.name
  disk = google_compute_disk.data.name
  zone = local.zone
}
