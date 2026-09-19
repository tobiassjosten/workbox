# Decode the shared config once and normalize it into locals. Defaults here must
# mirror internal/config/config.go Defaults() so the CLI and Terraform agree.
#
# For any value the CLI also consumes, use coalesce(try(local.cfg.X, ""),
# <default>) rather than bare try(): Defaults() normalizes an empty string — not
# only an absent key — to the default, so a present-but-empty value (e.g. X: "")
# must fall back too. A bare try() would let "" through and make the two sides
# disagree on what an empty value means.

locals {
  cfg = yamldecode(file(pathexpand(var.config_file)))

  name = local.cfg.name

  project = local.cfg.gcp.project_id
  region  = local.cfg.gcp.region
  zone    = local.cfg.gcp.zone

  # Region for Workflows and Cloud Scheduler. europe-north2 lacks both services,
  # so set gcp.services_region in config to a nearby supported region (e.g.
  # europe-west3). Falls back to the Terraform variable, then to gcp.region.
  services_region = coalesce(try(local.cfg.gcp.services_region, ""), var.services_region, local.region)

  machine_type = local.cfg.gcp.machine_type
  boot_disk_gb = local.cfg.gcp.boot_disk_gb
  data_disk_gb = local.cfg.gcp.data_disk_gb

  image_family  = coalesce(try(local.cfg.gcp.image_family, ""), "ubuntu-2404-lts-amd64")
  image_project = coalesce(try(local.cfg.gcp.image_project, ""), "ubuntu-os-cloud")

  deletion_protection = try(local.cfg.gcp.deletion_protection, true)

  linux_user     = local.cfg.machine.linux_user
  ssh_public_key = trimspace(file(pathexpand(local.cfg.machine.ssh_public_key_file)))
  data_mount     = coalesce(try(local.cfg.machine.data_mount, ""), "/work")

  ts_hostname      = local.cfg.tailscale.hostname
  ts_tag           = try(local.cfg.tailscale.tag, "tag:${local.name}")
  ts_user          = try(local.cfg.tailscale.user, "")
  ts_manage_policy = try(local.cfg.tailscale.manage_policy, false)

  # SSH destination for `workbox ssh` / the ssh_target output; mirrors
  # Defaults(), which falls back to the hostname when ssh_target is empty.
  ssh_target = coalesce(try(local.cfg.tailscale.ssh_target, ""), local.ts_hostname)

  timezone = local.cfg.schedule.timezone
  wake     = local.cfg.schedule.wake
  sleep    = local.cfg.schedule.sleep

  # Baseline wake/sleep as minutes-since-midnight for the reconciler.
  wake_min  = tonumber(split(":", local.wake)[0]) * 60 + tonumber(split(":", local.wake)[1])
  sleep_min = tonumber(split(":", local.sleep)[0]) * 60 + tonumber(split(":", local.sleep)[1])

  # Read by both the CLI and the reconciler; an empty-vs-absent mismatch here
  # would make them target different Firestore documents (see the note above).
  firestore_database = coalesce(try(local.cfg.state.firestore_database, ""), "(default)")
  firestore_location = coalesce(try(local.cfg.state.firestore_location, ""), "eur3")
  state_collection   = coalesce(try(local.cfg.state.collection, ""), "workbox")
  state_document     = coalesce(try(local.cfg.state.document, ""), local.name)

  # Stable device name -> /dev/disk/by-id/google-<device_name> on the guest.
  data_device_name = "${local.name}-data"

  labels = {
    managed-by = "workbox"
    component  = "workbox"
  }
}
