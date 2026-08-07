output "vm_external_ip" {
  description = "External IP address of the k3s server VM. Used internally by the root module to construct api_endpoint and control_plane_ip outputs."
  value       = google_compute_address.kyber_small.address
}

output "agent_log_bucket_name" {
  description = "Name of the GCS bucket holding shipped agent logs (kyber#431). Set this as logShipper.bucket / KYBER_LOG_ARCHIVE_BUCKET in the Helm values so the shipper writes here and the control-plane archive read path reads here."
  value       = google_storage_bucket.agent_logs.name
}

output "k3s_server_url" {
  description = "k3s API server URL (https://<external_ip>:6443)."
  value       = "https://${google_compute_address.kyber_small.address}:6443"
}

# The k3s join token lives on the VM at /var/lib/rancher/k3s/server/node-token.
# To retrieve it after apply, run:
#   gcloud compute ssh kyber-small-k3s-server \
#     --zone <zone> --project <project_id> \
#     --command 'sudo cat /var/lib/rancher/k3s/server/node-token'
output "k3s_join_token" {
  description = "Shell command to retrieve the k3s cluster join token from the running VM. Run the printed command after `terraform apply` finishes and k3s has bootstrapped. NOTE: this output is a retrieval instruction, not the token value itself, because the token only exists after k3s bootstraps on the VM. A future task may add a remote-exec provisioner to materialize the token directly."
  value       = "retrieve-after-apply: gcloud compute ssh ${google_compute_instance.k3s_server.name} --zone ${var.zone} --project ${var.project_id} --command 'sudo cat /var/lib/rancher/k3s/server/node-token'"
  sensitive   = true
}

# The kubeconfig lives on the VM at /etc/rancher/k3s/k3s.yaml (with 127.0.0.1 as server).
# To retrieve a working kubeconfig after apply, run:
#   gcloud compute ssh kyber-small-k3s-server \
#     --zone <zone> --project <project_id> \
#     --command 'sudo cat /etc/rancher/k3s/k3s.yaml' \
#   | sed "s/127.0.0.1/<external_ip>/"
output "kubeconfig" {
  description = "Shell command to retrieve the kubeconfig from the running k3s server VM. Run the printed command after `terraform apply` finishes and k3s has bootstrapped. NOTE: this output is a retrieval instruction, not the kubeconfig value itself, because the file only exists after k3s starts on the VM. A future task may add a remote-exec provisioner to materialize the kubeconfig directly."
  value       = "retrieve-after-apply: gcloud compute ssh ${google_compute_instance.k3s_server.name} --zone ${var.zone} --project ${var.project_id} --command 'sudo cat /etc/rancher/k3s/k3s.yaml' | sed \"s/127.0.0.1/${google_compute_address.kyber_small.address}/\""
  sensitive   = true
}
