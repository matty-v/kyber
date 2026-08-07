output "kubeconfig" {
  description = "Shell command to retrieve the kubeconfig from the running k3s server VM. Run the printed command after `terraform apply` finishes and k3s has bootstrapped. NOTE: this output is a retrieval instruction, not the kubeconfig value itself, because the file only exists after k3s starts on the VM. A future task may add a remote-exec provisioner to materialize the kubeconfig directly."
  value       = length(module.profile) > 0 ? module.profile[0].kubeconfig : null
  sensitive   = true
}

output "k3s_join_token" {
  description = "Shell command to retrieve the k3s cluster join token from the running VM. Run the printed command after `terraform apply` finishes and k3s has bootstrapped. NOTE: this output is a retrieval instruction, not the token value itself, because the token only exists after k3s bootstraps on the VM. A future task may add a remote-exec provisioner to materialize the token directly."
  value       = length(module.profile) > 0 ? module.profile[0].k3s_join_token : null
  sensitive   = true
}

output "k3s_server_url" {
  description = "k3s API server URL (https://<external_ip>:6443)."
  value       = length(module.profile) > 0 ? module.profile[0].k3s_server_url : null
}

output "api_endpoint" {
  description = "Kyber platform API endpoint (HTTP, port 8080). HTTPS and ingress are configured separately."
  value       = length(module.profile) > 0 ? "http://${module.profile[0].vm_external_ip}:8080" : null
}

output "control_plane_ip" {
  description = "External IP of the k3s control plane (same as the VM's static external IP for the small profile)."
  value       = length(module.profile) > 0 ? module.profile[0].vm_external_ip : null
}

output "agent_log_bucket_name" {
  description = "Name of the GCS bucket holding shipped agent logs (kyber#431). Set this as logShipper.bucket in the Helm values so the shipper writes here and the control-plane archive read path (source=archive) reads here."
  value       = length(module.profile) > 0 ? module.profile[0].agent_log_bucket_name : null
}
