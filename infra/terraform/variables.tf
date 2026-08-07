variable "project_id" {
  description = "GCP project ID to deploy into."
  type        = string
}

variable "profile" {
  description = "Infrastructure profile to deploy. Supported: small. (medium and large profiles are not yet implemented.)"
  type        = string
  default     = "small"

  validation {
    condition     = var.profile == "small"
    error_message = "Only the 'small' profile is currently implemented. Set profile = \"small\"."
  }
}

variable "region" {
  description = "GCP region."
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone (used for the small-profile VM and machine VMs)."
  type        = string
  default     = "us-central1-a"
}

variable "machine_type" {
  description = "GCE machine type for the small-profile k3s server VM."
  type        = string
  default     = "e2-medium"
}

variable "boot_disk_type" {
  description = "Boot disk type for the small-profile VM. pd-standard is fine for control-plane-only roles; pd-ssd costs ~4x more and is overkill at this scale."
  type        = string
  default     = "pd-standard"

  validation {
    condition     = contains(["pd-standard", "pd-balanced", "pd-ssd"], var.boot_disk_type)
    error_message = "boot_disk_type must be one of: pd-standard, pd-balanced, pd-ssd."
  }
}

variable "disk_size_gb" {
  description = "Boot disk size in GiB for the small-profile VM."
  type        = number
  default     = 100
}

variable "allowed_ssh_source_ranges" {
  description = "CIDR ranges allowed to reach the VM on port 22. Restrict this for production. Default allows all (0.0.0.0/0)."
  type        = list(string)
  default     = ["0.0.0.0/0"]
  # TODO: the spec does not specify source ranges; restricting to operator IP ranges is recommended before production use.
}

