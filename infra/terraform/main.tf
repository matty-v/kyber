# NOTE: This count-based profile selection assumes exactly one profile is
# active at a time. When medium and large profiles are added:
#   1. Relax the var.profile validation to accept the new values
#   2. Add parallel module blocks for each profile with their own count guards
#   3. Update outputs.tf to select from whichever module is active
# The current outputs rely on the small profile being the only one that can be
# count=1.
module "profile" {
  # The small profile is the only implemented profile (count enforces this via the variable validation).
  # When medium/large profiles are added, extend this block with additional count-guarded module calls.
  count = var.profile == "small" ? 1 : 0

  source = "./profiles/small"

  project_id                = var.project_id
  region                    = var.region
  zone                      = var.zone
  machine_type              = var.machine_type
  disk_size_gb              = var.disk_size_gb
  boot_disk_type            = var.boot_disk_type
  allowed_ssh_source_ranges = var.allowed_ssh_source_ranges
}
