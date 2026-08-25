variable "region" {
  type        = string
  description = "AWS region for the disposable qualification."
  default     = "us-east-2"

  validation {
    condition     = var.region == "us-east-2"
    error_message = "Phase 1 is approved only for us-east-2."
  }
}

variable "run_id" {
  type        = string
  description = "Unique lowercase run identifier included in every resource name and tag."

  validation {
    condition     = can(regex("^[a-z0-9]{6,20}$", var.run_id))
    error_message = "run_id must contain 6-20 lowercase letters or digits."
  }
}

variable "expires_at" {
  type        = string
  description = "RFC3339 cleanup deadline, at most eight hours after plan time."

  validation {
    condition = (
      can(timecmp(var.expires_at, timestamp())) &&
      timecmp(var.expires_at, timestamp()) > 0 &&
      timecmp(var.expires_at, timeadd(timestamp(), "8h")) <= 0
    )
    error_message = "expires_at must be in the future and no more than eight hours away."
  }
}

variable "instance_type" {
  type        = string
  description = "Bounded EC2 runner size."
  default     = "t3.large"

  validation {
    condition     = contains(["t3.medium", "t3.large"], var.instance_type)
    error_message = "Only t3.medium and t3.large are approved for Phase 1."
  }
}

