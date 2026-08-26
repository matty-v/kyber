output "account_id" {
  value = data.aws_caller_identity.current.account_id
}
output "region" {
  value = var.region
}
output "cluster_name" {
  value = aws_eks_cluster.this.name
}
output "cluster_endpoint" {
  value = aws_eks_cluster.this.endpoint
}
output "allowed_zones" {
  value = var.availability_zones
}
output "subnets_by_zone" {
  value = {
    for zone, subnet in aws_subnet.workers : zone => subnet.id
  }
}
output "node_role_arn" {
  value = aws_iam_role.node.arn
}
output "platform_launch_template_id" {
  value = aws_launch_template.platform.id
}
output "platform_launch_template_version" {
  value = aws_launch_template.platform.latest_version
}
output "machine_launch_template_id" {
  value = aws_launch_template.machine.id
}
output "machine_launch_template_version" {
  value = aws_launch_template.machine.latest_version
}
output "kyber_control_plane_role_arn" {
  value = aws_iam_role.kyber_control_plane.arn
}
