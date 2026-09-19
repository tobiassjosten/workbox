# A custom VPC with a single subnet. There are deliberately NO ingress firewall
# rules: GCP's implied "deny all ingress" stays in effect, so the VM has no
# public inbound surface. SSH arrives over Tailscale (an outbound connection),
# not through a 0.0.0.0/0 rule. The implied "allow all egress" rule provides
# outbound internet access via the instance's ephemeral external IP.

resource "google_compute_network" "workbox" {
  name                    = "${local.name}-net"
  auto_create_subnetworks = false
  depends_on              = [google_project_service.apis]
}

resource "google_compute_subnetwork" "workbox" {
  name          = "${local.name}-subnet"
  ip_cidr_range = "10.10.0.0/24"
  region        = local.region
  network       = google_compute_network.workbox.id

  # Private Google access lets the VM reach Google APIs without a public IP.
  private_ip_google_access = true
}
