# ---- Agent log archive bucket (kyber#431) -----------------------------------
#
# Durable, off-cluster store for agent stdout shipped by the log-shipper
# DaemonSet (deploy/helm/kyber/templates/log-shipper). Objects are keyed
#   agents/<agent>/<YYYY-MM-DD>/<ts>-<node>.ndjson
# and the control-plane archive read path (source=archive) lists strictly under
# the per-agent prefix, so one agent can never read another's logs.
#
# Credentials: BOTH the shipper and the read path authenticate via the node
# service account's Application Default Credentials (cloud-platform scope,
# already attached at k3s.tf service_account block) — NO static key material is
# created, mounted, or stored. Access is narrowed at the IAM layer to THIS
# bucket only (bucket-scoped, not project-wide), so a node compromise is bounded
# to this bucket's contents.

resource "google_storage_bucket" "agent_logs" {
  name     = coalesce(var.log_bucket_name, "${var.project_id}-${local.name_prefix}-agent-logs")
  project  = var.project_id
  location = var.region

  # Uniform bucket-level access: no per-object ACLs, no path to public exposure.
  uniform_bucket_level_access = true

  # Bounded retention — the AC's explicit storage-cost bound. GCS auto-deletes
  # objects older than the agreed window; no cron, no compaction job.
  lifecycle_rule {
    condition {
      age = var.log_retention_days
    }
    action {
      type = "Delete"
    }
  }

  # Shipped logs are reconstructable telemetry, not a source of record — no
  # object versioning. force_destroy lets `terraform destroy` reclaim the bucket
  # without a manual object purge (this is the small single-node profile).
  force_destroy = true

  labels = {
    managed-by    = "kyber-terraform"
    kyber-profile = "small"
    kyber-purpose = "agent-logs"
  }
}

# Bucket-scoped objectAdmin for the existing node SA. The shipper writes objects
# and the control-plane reads them, both as this SA over ADC. Scoped to the
# single bucket (NOT roles/storage.* at the project level) per the design's
# least-privilege requirement — mirror of the project IAM members in k3s.tf,
# but using google_storage_bucket_iam_member so the grant is bucket-local.
resource "google_storage_bucket_iam_member" "agent_logs_node_sa" {
  bucket = google_storage_bucket.agent_logs.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.kyber_small.email}"
}
