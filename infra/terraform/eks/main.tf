locals {
  # IAM appends a unique suffix to name_prefix and therefore limits it to 38
  # characters. Keep role names valid even when the EKS cluster name is long.
  iam_name_stem = substr(var.cluster_name, 0, 20)
  tags = {
    "kyber.io/managed-by" = "terraform", "kyber.io/owner" = var.owner, "kyber.io/run-id" = var.run_id, "kyber.io/issue" = "103", "kyber.io/expires-at" = var.expires_at
  }
}
data "aws_caller_identity" "current" {}

resource "aws_vpc" "this" {
  cidr_block           = "10.83.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = "${var.cluster_name}-vpc" }
}
resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
}
resource "aws_subnet" "workers" {
  for_each                = toset(var.availability_zones)
  vpc_id                  = aws_vpc.this.id
  availability_zone       = each.value
  cidr_block              = cidrsubnet(aws_vpc.this.cidr_block, 8, index(var.availability_zones, each.value))
  map_public_ip_on_launch = true
  tags                    = { Name = "${var.cluster_name}-${each.value}", "kubernetes.io/cluster/${var.cluster_name}" = "shared" }
}
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
}
resource "aws_route_table_association" "workers" {
  for_each       = aws_subnet.workers
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

data "aws_iam_policy_document" "cluster_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}
resource "aws_iam_role" "cluster" {
  name_prefix        = "${local.iam_name_stem}-cluster-"
  assume_role_policy = data.aws_iam_policy_document.cluster_assume.json
}
resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

resource "aws_eks_cluster" "this" {
  name                      = var.cluster_name
  role_arn                  = aws_iam_role.cluster.arn
  enabled_cluster_log_types = ["api", "audit", "authenticator", "controllerManager", "scheduler"]
  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }
  vpc_config {
    subnet_ids              = values(aws_subnet.workers)[*].id
    endpoint_private_access = true
    endpoint_public_access  = true
    public_access_cidrs     = var.public_access_cidrs
  }
  depends_on = [aws_iam_role_policy_attachment.cluster]
}

data "aws_iam_policy_document" "node_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}
resource "aws_iam_role" "node" {
  name_prefix        = "${local.iam_name_stem}-node-"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json
}
resource "aws_iam_role_policy_attachment" "node_worker" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
}
resource "aws_iam_role_policy_attachment" "node_ecr" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly"
}
resource "aws_iam_role_policy_attachment" "node_cni" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
}
resource "aws_launch_template" "platform" {
  name_prefix = "${var.cluster_name}-platform-"
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      encrypted             = true
      volume_type           = "gp3"
      volume_size           = 50
      delete_on_termination = true
    }
  }
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  tag_specifications {
    resource_type = "instance"
    tags          = local.tags
  }
}

resource "aws_launch_template" "machine" {
  name_prefix = "${var.cluster_name}-machine-"
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      encrypted             = true
      volume_type           = "gp3"
      volume_size           = var.machine_root_disk_size
      delete_on_termination = true
    }
  }
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  tag_specifications {
    resource_type = "instance"
    tags          = local.tags
  }
}
resource "aws_eks_node_group" "platform" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "kyber-platform"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = [aws_subnet.workers[var.availability_zones[0]].id]
  capacity_type   = "ON_DEMAND"
  instance_types  = [var.platform_instance_type]
  scaling_config {
    desired_size = 1
    min_size     = 1
    max_size     = 1
  }
  launch_template {
    id      = aws_launch_template.platform.id
    version = tostring(aws_launch_template.platform.latest_version)
  }
  labels     = { "kyber.io/role" = "platform" }
  depends_on = [aws_iam_role_policy_attachment.node_worker, aws_iam_role_policy_attachment.node_ecr, aws_iam_role_policy_attachment.node_cni]
}

resource "aws_eks_addon" "vpc_cni" {
  cluster_name = aws_eks_cluster.this.name
  addon_name   = "vpc-cni"
}
resource "aws_eks_addon" "coredns" {
  cluster_name = aws_eks_cluster.this.name
  addon_name   = "coredns"
  depends_on   = [aws_eks_node_group.platform]
}
resource "aws_eks_addon" "kube_proxy" {
  cluster_name = aws_eks_cluster.this.name
  addon_name   = "kube-proxy"
}
resource "aws_eks_addon" "pod_identity" {
  cluster_name = aws_eks_cluster.this.name
  addon_name   = "eks-pod-identity-agent"
  depends_on   = [aws_eks_node_group.platform]
}
data "aws_iam_policy_document" "pod_assume" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}
resource "aws_iam_role" "ebs_csi" {
  name_prefix        = "${local.iam_name_stem}-ebs-csi-"
  assume_role_policy = data.aws_iam_policy_document.pod_assume.json
}
resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}
resource "aws_eks_addon" "ebs_csi" {
  cluster_name = aws_eks_cluster.this.name
  addon_name   = "aws-ebs-csi-driver"
  depends_on   = [aws_eks_pod_identity_association.ebs_csi]
}
resource "aws_eks_pod_identity_association" "ebs_csi" {
  cluster_name    = aws_eks_cluster.this.name
  namespace       = "kube-system"
  service_account = "ebs-csi-controller-sa"
  role_arn        = aws_iam_role.ebs_csi.arn
  depends_on      = [aws_eks_addon.pod_identity, aws_iam_role_policy_attachment.ebs_csi]
}

resource "aws_iam_role" "kyber_control_plane" {
  name_prefix        = "${local.iam_name_stem}-kyber-cp-"
  assume_role_policy = data.aws_iam_policy_document.pod_assume.json
}

data "aws_iam_policy_document" "kyber_control_plane" {
  statement {
    sid       = "ClusterRead"
    actions   = ["eks:DescribeCluster"]
    resources = [aws_eks_cluster.this.arn]
  }
  statement {
    sid = "CreateOwnedNodegroups"
    # CreateNodegroup evaluates eks:TagResource against the parent cluster ARN
    # when tags are supplied with the request. Keep both actions behind the
    # Kyber ownership-tag conditions so the controller cannot tag arbitrarily.
    actions   = ["eks:CreateNodegroup", "eks:TagResource"]
    resources = [aws_eks_cluster.this.arn]
    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/kyber.io/managed-by"
      values   = ["kyber"]
    }
    condition {
      test     = "Null"
      variable = "aws:RequestTag/kyber.io/machine"
      values   = ["false"]
    }
  }
  statement {
    sid = "OwnedNodegroupLifecycle"
    actions = [
      "eks:DescribeNodegroup",
      "eks:UpdateNodegroupConfig",
      "eks:DeleteNodegroup",
      "eks:TagResource",
    ]
    resources = ["arn:aws:eks:${var.region}:${data.aws_caller_identity.current.account_id}:nodegroup/${aws_eks_cluster.this.name}/kyber-*/*"]
  }
  statement {
    sid       = "PassMachineNodeRole"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.node.arn]
    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["eks.amazonaws.com"]
    }
  }
  statement {
    sid = "ReadEKSServiceRoles"
    actions = [
      "iam:GetRole",
      "iam:ListAttachedRolePolicies",
      "iam:ListRolePolicies",
      "iam:GetRolePolicy",
    ]
    resources = [
      aws_iam_role.cluster.arn,
      aws_iam_role.node.arn,
    ]
  }
  statement {
    sid = "ReadEKSManagedPolicies"
    actions = [
      "iam:GetPolicy",
      "iam:GetPolicyVersion",
    ]
    resources = [
      "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
      "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
      "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly",
      "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
    ]
  }
  statement {
    sid       = "ReadApprovedLaunchTemplate"
    actions   = ["ec2:DescribeLaunchTemplateVersions"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "kyber_control_plane" {
  role   = aws_iam_role.kyber_control_plane.id
  policy = data.aws_iam_policy_document.kyber_control_plane.json
}

resource "aws_eks_pod_identity_association" "kyber_control_plane" {
  cluster_name    = aws_eks_cluster.this.name
  namespace       = var.kyber_namespace
  service_account = var.kyber_control_plane_service_account
  role_arn        = aws_iam_role.kyber_control_plane.arn
  depends_on      = [aws_eks_addon.pod_identity, aws_iam_role_policy.kyber_control_plane]
}
