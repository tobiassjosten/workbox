# The only required input is the path to the shared workbox config, which both
# the CLI and Terraform read so there is a single source of truth. No secrets
# are passed as variables: GCP auth comes from ADC and Tailscale auth from
# environment variables (see providers.tf).

variable "config_file" {
  type        = string
  description = "Path to the workbox config.yaml (defaults to ~/.config/workbox/config.yaml)."
  default     = "~/.config/workbox/config.yaml"
}

variable "services_region" {
  type        = string
  description = "Region for Workflows and Cloud Scheduler. Leave empty to reuse gcp.region; set this if that region lacks Workflows/Scheduler."
  default     = ""
}

variable "reconcile_interval" {
  type        = string
  description = "Cloud Scheduler cron for the reconciler. Every minute keeps idle-suspend punctual (the 5-minute idle_timeout_minutes minimum assumes it) and stays within free tiers."
  default     = "* * * * *"
  validation {
    condition     = length(trimspace(var.reconcile_interval)) > 0
    error_message = "reconcile_interval must be a non-empty Cloud Scheduler cron expression."
  }
}

variable "snapshot_retention_days" {
  type        = number
  description = "How many days of daily development-disk snapshots to keep."
  default     = 14
  validation {
    condition     = var.snapshot_retention_days > 0
    error_message = "snapshot_retention_days must be greater than 0."
  }
}

variable "snapshot_time_utc" {
  type        = string
  description = "Daily snapshot start time (UTC, HH:MM)."
  default     = "02:00"
  validation {
    condition     = can(regex("^([01][0-9]|2[0-3]):[0-5][0-9]$", var.snapshot_time_utc))
    error_message = "snapshot_time_utc must be HH:MM in 24-hour UTC format."
  }
}
