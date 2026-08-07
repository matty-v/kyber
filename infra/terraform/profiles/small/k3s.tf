# ---- Network ----------------------------------------------------------------

resource "google_compute_network" "kyber_small" {
  name                    = "${local.name_prefix}-network"
  auto_create_subnetworks = false
  project                 = var.project_id
}

resource "google_compute_subnetwork" "kyber_small" {
  name          = "${local.name_prefix}-subnet"
  ip_cidr_range = "10.10.0.0/24"
  region        = var.region
  network       = google_compute_network.kyber_small.id
  project       = var.project_id
}

# ---- Static external IP -----------------------------------------------------

resource "google_compute_address" "kyber_small" {
  name    = "${local.name_prefix}-ip"
  region  = var.region
  project = var.project_id
}

# ---- Firewall rules ---------------------------------------------------------

resource "google_compute_firewall" "allow_ssh" {
  name    = "${local.name_prefix}-allow-ssh"
  network = google_compute_network.kyber_small.name
  project = var.project_id

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = var.allowed_ssh_source_ranges
  target_tags   = ["k3s-server"]

  description = "Allow SSH access to the k3s server VM. Restrict source_ranges for production."
}

resource "google_compute_firewall" "allow_k3s_api" {
  name    = "${local.name_prefix}-allow-k3s-api"
  network = google_compute_network.kyber_small.name
  project = var.project_id

  allow {
    protocol = "tcp"
    ports    = ["6443"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["k3s-server"]

  description = "Allow k3s API server access (port 6443). Required by Machine Controller and kubectl."
}

resource "google_compute_firewall" "allow_kyber_api" {
  name    = "${local.name_prefix}-allow-kyber-api"
  network = google_compute_network.kyber_small.name
  project = var.project_id

  allow {
    protocol = "tcp"
    ports    = ["8080"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["k3s-server"]

  description = "Allow Kyber platform API access (port 8080)."
}

resource "google_compute_firewall" "allow_https" {
  name    = "${local.name_prefix}-allow-https"
  network = google_compute_network.kyber_small.name
  project = var.project_id

  allow {
    protocol = "tcp"
    ports    = ["443"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["k3s-server"]

  description = "Allow HTTPS inbound for ingress."
}

resource "google_compute_firewall" "allow_icmp" {
  name    = "${local.name_prefix}-allow-icmp"
  network = google_compute_network.kyber_small.name
  project = var.project_id

  allow {
    protocol = "icmp"
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["k3s-server"]

  description = "Allow ICMP for health checks and connectivity testing."
}

resource "google_compute_firewall" "allow_internal" {
  name    = "${local.name_prefix}-allow-internal"
  network = google_compute_network.kyber_small.name
  project = var.project_id

  allow {
    protocol = "tcp"
    ports    = ["0-65535"]
  }

  allow {
    protocol = "udp"
    ports    = ["0-65535"]
  }

  allow {
    protocol = "icmp"
  }

  source_ranges = ["10.10.0.0/24"]

  description = "Allow all traffic within the kyber-small subnet (k3s node communication)."
}

# ---- Service account --------------------------------------------------------

resource "google_service_account" "kyber_small" {
  account_id   = "${local.name_prefix}-sa"
  display_name = "Kyber Small Profile Service Account"
  project      = var.project_id
}

# Allow the VM to write logs and pull images from GCR/Artifact Registry.
resource "google_project_iam_member" "kyber_small_log_writer" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.kyber_small.email}"
}

resource "google_project_iam_member" "kyber_small_metric_writer" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.kyber_small.email}"
}

resource "google_project_iam_member" "kyber_small_artifact_reader" {
  project = var.project_id
  role    = "roles/artifactregistry.reader"
  member  = "serviceAccount:${google_service_account.kyber_small.email}"
}

# The k3s control plane needs to create/manage GCE VMs for Kyber machine nodes.
# TODO(prod): narrow to a custom role with only the compute actions the Machine Controller actually uses
resource "google_project_iam_member" "kyber_small_compute_admin" {
  project = var.project_id
  role    = "roles/compute.instanceAdmin.v1"
  member  = "serviceAccount:${google_service_account.kyber_small.email}"
}

# ---- GCE VM -----------------------------------------------------------------

resource "google_compute_instance" "k3s_server" {
  name         = "${local.name_prefix}-k3s-server"
  machine_type = var.machine_type
  zone         = var.zone
  project      = var.project_id

  tags = ["kyber-small", "k3s-server"]
  labels = {
    managed-by    = "kyber-terraform"
    kyber-profile = "small"
  }

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = var.disk_size_gb
      type  = var.boot_disk_type
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.kyber_small.id

    access_config {
      nat_ip = google_compute_address.kyber_small.address
    }
  }

  # No instance metadata is needed — the startup script is self-contained.
  # PostgreSQL, Redis, and the Kyber control plane are installed via Helm
  # from the operator's workstation (see docs/installation.md).
  metadata_startup_script = file("${path.module}/../../scripts/k3s-install.sh")

  service_account {
    email = google_service_account.kyber_small.email
    # OAuth scope set to cloud-platform so tokens can reach all Google APIs.
    # Actual access control is enforced by the IAM roles attached to the service
    # account above (logging, monitoring, artifact registry, compute.instanceAdmin).
    # This is not "admin everywhere" — it is the standard pattern for service
    # accounts that use IAM roles for authorization.
    scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]
  }
}
