# The development VM. It boots from a small OS disk, mounts the persistent
# development disk by stable /dev/disk/by-id path, joins Tailscale with a
# single-use tagged key, and configures SSH for key-only access. It carries the
# least-privilege VM service account — never the default Editor SA.

resource "google_compute_instance" "workbox" {
  name         = local.name
  machine_type = local.machine_type
  zone         = local.zone

  # Let Terraform stop the VM to apply changes such as machine_type. Set
  # deletion_protection from config to guard against accidental destroy.
  allow_stopping_for_update = true
  deletion_protection       = local.deletion_protection

  labels = local.labels
  tags   = ["workbox"]

  boot_disk {
    auto_delete = true
    initialize_params {
      size  = local.boot_disk_gb
      type  = "pd-balanced"
      image = "projects/${local.image_project}/global/images/family/${local.image_family}"
    }
  }

  # Persistent development disk, attached with a stable device name so the guest
  # can mount /dev/disk/by-id/google-${data_device_name}. auto_delete is false
  # and the disk itself has prevent_destroy.
  attached_disk {
    source      = google_compute_disk.data.id
    device_name = local.data_device_name
    mode        = "READ_WRITE"
  }

  network_interface {
    subnetwork = google_compute_subnetwork.workbox.id
    # Ephemeral external IP for outbound internet (package installs, Tailscale
    # coordination). No inbound firewall rule exposes it.
    access_config {}
  }

  # Suspend/resume preserves memory; MIGRATE avoids unnecessary terminations.
  scheduling {
    on_host_maintenance = "MIGRATE"
    automatic_restart   = true
    provisioning_model  = "STANDARD"
  }

  service_account {
    email  = google_service_account.vm.email
    scopes = ["logging-write", "monitoring-write"]
  }

  metadata = {
    # Only the public key is installed; no private key ever reaches the VM.
    ssh-keys               = "${local.linux_user}:${local.ssh_public_key}"
    block-project-ssh-keys = "true"
    enable-oslogin         = "false"

    # The single-use Tailscale enrollment key, in its own entry so the startup
    # script can stay Terraform-managed while this one is ignored after create
    # (see lifecycle below). The script reads it only when not yet enrolled.
    (local.ts_authkey_attr) = tailscale_tailnet_key.bootstrap.key

    startup-script = templatefile("${path.module}/cloud-init.sh.tftpl", {
      linux_user       = local.linux_user
      data_device_name = local.data_device_name
      data_mount       = local.data_mount
      timezone         = local.timezone
      ts_hostname      = local.ts_hostname
      ts_authkey_attr  = local.ts_authkey_attr
    })
  }

  # The tag must exist in the tailnet policy before the tagged key is used.
  depends_on = [
    google_project_service.apis,
    tailscale_tailnet_key.bootstrap,
  ]

  # NOTE: the startup-script is intentionally Terraform-managed (not ignored) so
  # provisioning changes reach the VM on the next `terraform apply`. It carries
  # no secret: the enrollment key lives in its own metadata entry, which is
  # ignored after create. A replaced key (any change to its arguments, or a
  # -replace) therefore never lands in a running, already enrolled VM's
  # metadata, where anything on the VM could read and use it.
  #
  # Metadata changes apply in place, but GCE runs a startup-script only at BOOT:
  # an updated script takes effect on the instance's next boot (a reboot, or a
  # stop/start — not on apply, and not on suspend/resume). After an apply that
  # changes provisioning, reboot or cold-restart the VM once (see
  # docs/operations.md, "Applying provisioning changes").

  lifecycle {
    # Must equal local.ts_authkey_attr (ignore_changes cannot reference a local).
    # The precondition below catches a change to the local, and config_test's
    # TestTailscaleKeyEntryIsIgnored catches either side changing alone.
    ignore_changes = [metadata["workbox-ts-authkey"]]

    precondition {
      condition     = local.ts_authkey_attr == "workbox-ts-authkey"
      error_message = "local.ts_authkey_attr must match the ignore_changes literal in compute.tf, or a replaced enrollment key would reach the running VM."
    }
  }
}
