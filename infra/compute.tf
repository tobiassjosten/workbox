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

    startup-script = templatefile("${path.module}/cloud-init.sh.tftpl", {
      linux_user       = local.linux_user
      data_device_name = local.data_device_name
      data_mount       = local.data_mount
      timezone         = local.timezone
      ts_hostname      = local.ts_hostname
      ts_authkey       = tailscale_tailnet_key.bootstrap.key
    })
  }

  # The tag must exist in the tailnet policy before the tagged key is used.
  depends_on = [
    google_project_service.apis,
    tailscale_tailnet_key.bootstrap,
  ]

  lifecycle {
    # The startup-script embeds a single-use auth key that is consumed on first
    # boot; don't recreate the VM just because the key value later changes.
    ignore_changes = [metadata["startup-script"]]
  }
}
