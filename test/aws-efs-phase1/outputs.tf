output "instance_id" {
  value = aws_instance.runner.id
}

output "efs_file_system_id" {
  value = aws_efs_file_system.phase1.id
}

output "efs_access_point_id" {
  value = aws_efs_access_point.phase1.id
}

output "efs_mount_target_ip" {
  value = aws_efs_mount_target.phase1.ip_address
}

output "ebs_volume_id" {
  value = aws_ebs_volume.control.id
}

output "run_id" {
  value = var.run_id
}
