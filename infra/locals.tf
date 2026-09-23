# Decode the shared config once and normalize it into locals. Defaults here must
# mirror internal/config's defaults (Defaults(), defaultIdleTimeoutMinutes, ...)
# so the CLI and Terraform agree; values Terraform owns alone are marked inline.
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

  # coalesce (not a bare try) so an explicit YAML null defaults to true as it
  # does in config.Defaults(); an explicit false still disables protection.
  deletion_protection = coalesce(try(local.cfg.gcp.deletion_protection, null), true)

  linux_user     = local.cfg.machine.linux_user
  ssh_public_key = trimspace(file(pathexpand(local.cfg.machine.ssh_public_key_file)))
  data_mount     = coalesce(try(local.cfg.machine.data_mount, ""), "/work")

  # Swapfile size (GB) the VM creates on boot so memory spikes degrade into
  # slowness instead of OOM-killing sessions. 0 disables. Consumed only here (the
  # Go CLI merely validates machine.swap_gb); coalesce so a YAML null also defaults.
  swap_gb = coalesce(try(local.cfg.machine.swap_gb, null), 4)

  ts_hostname      = local.cfg.tailscale.hostname
  ts_tag           = try(local.cfg.tailscale.tag, "tag:${local.name}")
  ts_user          = try(local.cfg.tailscale.user, "")
  ts_manage_policy = try(local.cfg.tailscale.manage_policy, false)
  # Instance metadata key holding the enrollment key; must match the literal in
  # compute.tf's ignore_changes (which cannot reference a local).
  ts_authkey_attr = "workbox-ts-authkey"

  # SSH destination for `workbox ssh` / the ssh_target output; mirrors
  # Defaults(), which falls back to the hostname when ssh_target is empty.
  ssh_target = coalesce(try(local.cfg.tailscale.ssh_target, ""), local.ts_hostname)

  timezone = local.cfg.schedule.timezone

  # Working hours: an optional recurring window during which auto-suspend is
  # disabled. Mirror internal/config: a present block is active unless enabled is
  # explicitly false. A disabled window is signalled to the reconciler with -1.
  work_hours   = try(local.cfg.schedule.working_hours, null)
  work_enabled = local.work_hours != null && coalesce(try(local.work_hours.enabled, null), true)
  work_start   = try(local.work_hours.start, "")
  work_end     = try(local.work_hours.end, "")
  # HH:MM, as internal/schedule.ParseDayTime accepts; a malformed value yields
  # -1 here and is reported by the precondition in workflow.tf.
  hhmm_re       = "^([01]?[0-9]|2[0-3]):[0-5]?[0-9]$"
  work_times_ok = can(regex(local.hhmm_re, local.work_start)) && can(regex(local.hhmm_re, local.work_end))
  work_start_min = local.work_enabled && local.work_times_ok ? (
    tonumber(split(":", local.work_start)[0]) * 60 + tonumber(split(":", local.work_start)[1])
  ) : -1
  work_end_min = local.work_enabled && local.work_times_ok ? (
    tonumber(split(":", local.work_end)[0]) * 60 + tonumber(split(":", local.work_end)[1])
  ) : -1

  # Weekdays the working-hours window is active on, as Go weekday numbers
  # (Sun=0..Sat=6, matching time.Weekday). Absent -> every day. Mirrors the
  # weekday parsing in internal/config; a YAML null (`days:`) also means every day.
  work_day_names = coalesce(try(local.work_hours.days, null), [])
  work_day_num = {
    sun = 0, sunday = 0
    mon = 1, monday = 1
    tue = 2, tues = 2, tuesday = 2
    wed = 3, weds = 3, wednesday = 3
    thu = 4, thur = 4, thurs = 4, thursday = 4
    fri = 5, friday = 5
    sat = 6, saturday = 6
  }
  # Normalized once so the lookup, the filter and the precondition agree.
  work_day_keys = [for d in local.work_day_names : lower(trimspace(d))]
  work_days_unknown = [
    for i, k in local.work_day_keys : local.work_day_names[i]
    if !contains(keys(local.work_day_num), k)
  ]
  work_days = local.work_enabled ? (
    length(local.work_day_keys) > 0
    # Unknown names are skipped here (not indexed) so the precondition in
    # workflow.tf is reached and reports them clearly.
    ? [for k in local.work_day_keys : local.work_day_num[k] if contains(keys(local.work_day_num), k)]
    : [0, 1, 2, 3, 4, 5, 6]
  ) : []
  # Set form the reconciler checks weekday membership against: {"1": true, ...}.
  # distinct() so repeated spellings of one day (e.g. [mon, monday]) collapse as
  # they do in config.Weekdays, instead of failing with a duplicate-key error.
  work_days_map = { for d in distinct(local.work_days) : tostring(d) => true }

  # Idle-shutdown timeout in minutes. Absent -> 30 (mirrors internal/config);
  # an explicit 0 disables idle shutdown. coalesce (not a bare try default) so a
  # YAML null (`idle_timeout_minutes:`) also defaults, as it does in Go.
  idle_min = coalesce(try(local.cfg.schedule.idle_timeout_minutes, null), 30)

  # Read by both the CLI and the reconciler; an empty-vs-absent mismatch here
  # would make them target different Firestore documents (see the note above).
  firestore_database = coalesce(try(local.cfg.state.firestore_database, ""), "(default)")
  firestore_location = coalesce(try(local.cfg.state.firestore_location, ""), "eur3")
  state_collection   = coalesce(try(local.cfg.state.collection, ""), "workbox")
  state_document     = coalesce(try(local.cfg.state.document, ""), local.name)

  # Stable device name -> /dev/disk/by-id/google-<device_name> on the guest.
  data_device_name = "${local.name}-data"

  # Guest-attributes path the VM activity emitter writes and the reconciler reads
  # (unix seconds since the VM was last active). This is the contract with
  # internal/compute.LastActiveQueryPath — change both together.
  activity_key = "workbox/last_active"
  # The key alone, as it appears in a getGuestAttributes response item.
  activity_key_name = split("/", local.activity_key)[1]

  labels = {
    managed-by = "workbox"
    component  = "workbox"
  }
}
