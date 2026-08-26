variable "region" {
  type    = string
  default = "us-east-1"
}
variable "cluster_name" {
  type    = string
  default = "kyber-eks-qualification"
}
variable "availability_zones" {
  type    = list(string)
  default = ["us-east-1a", "us-east-1b"]
  validation {
    condition     = length(var.availability_zones) == 2
    error_message = "Exactly two AZs are required."
  }
}
variable "owner" {
  type = string
  validation {
    condition     = trimspace(var.owner) != ""
    error_message = "owner is required."
  }
}
variable "run_id" {
  type = string
  validation {
    condition     = trimspace(var.run_id) != ""
    error_message = "run_id is required."
  }
}
variable "expires_at" {
  type = string
  validation {
    condition     = can(timecmp(var.expires_at, timestamp()))
    error_message = "expires_at must be RFC3339."
  }
}
variable "platform_instance_type" {
  type    = string
  default = "m7i.large"
}
variable "public_access_cidrs" {
  type        = list(string)
  description = "Reviewed CIDRs allowed to reach the public EKS API."
  validation {
    condition     = length(var.public_access_cidrs) > 0 && !contains(var.public_access_cidrs, "0.0.0.0/0")
    error_message = "Provide at least one bounded CIDR; 0.0.0.0/0 is forbidden."
  }
}

variable "kyber_namespace" {
  type    = string
  default = "kyber"
}

variable "kyber_control_plane_service_account" {
  type    = string
  default = "kyber-control-plane"
}
