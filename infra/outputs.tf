output "instance_name" {
  description = "Compute Engine instance name (also the workbox CLI target)."
  value       = google_compute_instance.workbox.name
}

output "zone" {
  description = "Instance zone."
  value       = local.zone
}

output "external_ip" {
  description = "Ephemeral external IP (outbound only; no public ingress)."
  value       = google_compute_instance.workbox.network_interface[0].access_config[0].nat_ip
}

output "tailscale_hostname" {
  description = "Tailscale MagicDNS hostname to use as the SSH target."
  value       = local.ts_hostname
}

output "ssh_target" {
  description = "Recommended SSH destination for `workbox ssh` / your ~/.ssh/config."
  value       = local.ssh_target
}

output "data_disk" {
  description = "Persistent development disk (protected by prevent_destroy)."
  value       = google_compute_disk.data.name
}

output "reconciler_workflow" {
  description = "Reconciler Workflow name."
  value       = google_workflows_workflow.reconcile.name
}

output "operator_role" {
  description = "Custom role to grant your human/CLI identity for day-to-day use."
  value       = google_project_iam_custom_role.operator.id
}

output "next_steps" {
  description = "What to do after apply."
  value       = <<-EOT
    workbox is provisioned. Next:
      1. Grant yourself the operator role:
           gcloud projects add-iam-policy-binding ${local.project} \
             --member="user:YOUR_EMAIL" --role="${google_project_iam_custom_role.operator.id}"
      2. If tailscale.manage_policy is false, apply the policy fragment in
         infra/tailscale-policy.example.hujson to your tailnet by hand.
      3. Install the CLI:      make install
      4. First connection:     workbox ssh   then run: claude   (interactive login)
      5. Daily use:            workbox
  EOT
}
