# Firestore in Native mode holds the tiny operational-state document read by the
# Workflow connector and written by the CLI. No project data lives here; access
# is by IAM only. The database location is set once at creation and cannot change.

resource "google_firestore_database" "state" {
  project     = local.project
  name        = local.firestore_database
  location_id = local.firestore_location
  type        = "FIRESTORE_NATIVE"

  # Guard the operational-state store against accidental deletion.
  deletion_policy = "ABANDON"

  depends_on = [google_project_service.apis]
}
