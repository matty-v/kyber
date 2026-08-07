variable "project_id" {
  description = "GCP project ID."
  type        = string
}

# ---- Agent log archive (kyber#431) -----------------------------------------

variable "log_retention_days" {
  description = "Days to retain shipped agent logs in the archive bucket before GCS lifecycle auto-deletes them. Bounds storage cost; reach-back of the source=archive read path. Default 30 (enough to reconstruct a multi-day feature; Matt's call per the design's open question #1)."
  type        = number
  default     = 30
}

variable "log_bucket_name" {
  description = "Override the agent-log archive bucket name. Empty (default) derives a unique name: \"<project_id>-kyber-small-agent-logs\". Set explicitly only if that derived name collides or a specific name is required."
  type        = string
  default     = ""
}

variable "region" {
  description = "GCP region."
  type        = string
}

variable "zone" {
  description = "GCP zone for the k3s server VM."
  type        = string
}

variable "machine_type" {
  description = "GCE machine type for the k3s server VM."
  type        = string
}

variable "disk_size_gb" {
  description = "Boot disk size in GiB."
  type        = number
}

variable "boot_disk_type" {
  description = "Boot disk type. pd-standard for cheap control-plane-only roles; pd-ssd for IOPS-sensitive workloads."
  type        = string
}

variable "allowed_ssh_source_ranges" {
  description = "CIDR ranges allowed to reach the VM on port 22."
  type        = list(string)
  # No default — value is always supplied by the root module (var.allowed_ssh_source_ranges).
}
