terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = local.tags
  }
}

locals {
  name = "kyber-efs-phase1-${var.run_id}"
  tags = {
    Name                  = local.name
    "kyber.io/managed-by" = "phase1-qualification"
    "kyber.io/issue"      = "103"
    "kyber.io/run-id"     = var.run_id
    "kyber.io/expires-at" = var.expires_at
  }
}

data "aws_ami" "amazon_linux" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-x86_64"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
  }

  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }
}

resource "aws_vpc" "phase1" {
  cidr_block           = "10.103.0.0/24"
  enable_dns_hostnames = true
  enable_dns_support   = true
}

resource "aws_internet_gateway" "phase1" {
  vpc_id = aws_vpc.phase1.id
}

resource "aws_subnet" "phase1" {
  vpc_id                  = aws_vpc.phase1.id
  cidr_block              = "10.103.0.0/25"
  availability_zone       = "${var.region}a"
  map_public_ip_on_launch = true
}

resource "aws_route_table" "phase1" {
  vpc_id = aws_vpc.phase1.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.phase1.id
  }
}

resource "aws_route_table_association" "phase1" {
  subnet_id      = aws_subnet.phase1.id
  route_table_id = aws_route_table.phase1.id
}

resource "aws_security_group" "runner" {
  name        = "${local.name}-runner"
  description = "No-ingress runner for Kyber EFS qualification"
  vpc_id      = aws_vpc.phase1.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "efs" {
  name        = "${local.name}-efs"
  description = "NFS only from the Phase 1 runner"
  vpc_id      = aws_vpc.phase1.id

  ingress {
    from_port       = 2049
    to_port         = 2049
    protocol        = "tcp"
    security_groups = [aws_security_group.runner.id]
  }
}

resource "aws_efs_file_system" "phase1" {
  encrypted        = true
  performance_mode = "generalPurpose"
  throughput_mode  = "elastic"

  lifecycle_policy {
    transition_to_ia = "AFTER_30_DAYS"
  }
}

resource "aws_efs_mount_target" "phase1" {
  file_system_id  = aws_efs_file_system.phase1.id
  subnet_id       = aws_subnet.phase1.id
  security_groups = [aws_security_group.efs.id]
}

resource "aws_efs_access_point" "phase1" {
  file_system_id = aws_efs_file_system.phase1.id

  root_directory {
    path = "/qualification"

    creation_info {
      owner_gid   = 0
      owner_uid   = 0
      permissions = "0755"
    }
  }
}

resource "aws_iam_role" "runner" {
  name = "${local.name}-runner"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.runner.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "runner" {
  name = "${local.name}-runner"
  role = aws_iam_role.runner.name
}

resource "aws_instance" "runner" {
  ami                         = data.aws_ami.amazon_linux.id
  instance_type               = var.instance_type
  subnet_id                   = aws_subnet.phase1.id
  associate_public_ip_address = true
  vpc_security_group_ids      = [aws_security_group.runner.id]
  iam_instance_profile        = aws_iam_instance_profile.runner.name

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  root_block_device {
    encrypted   = true
    volume_size = 20
    volume_type = "gp3"
  }

  user_data = <<-USERDATA
    #!/bin/bash
    set -euxo pipefail
    dnf install -y amazon-efs-utils attr docker git jq time
    systemctl enable --now docker amazon-ssm-agent
    touch /var/lib/kyber-phase1-ready
  USERDATA

  depends_on = [aws_iam_role_policy_attachment.ssm]
}

resource "aws_ebs_volume" "control" {
  availability_zone = aws_subnet.phase1.availability_zone
  encrypted         = true
  size              = 20
  type              = "gp3"
}

resource "aws_volume_attachment" "control" {
  device_name = "/dev/sdf"
  instance_id = aws_instance.runner.id
  volume_id   = aws_ebs_volume.control.id
}

