# skybridge/aws-compute v0.1.0 — source EKS reference footprint.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

variable "name" {
  type = string
}

variable "role_arn" {
  type        = string
  description = "Cluster service role. Attached via OIDC-assumed identity, never static keys."
}

variable "subnet_ids" {
  type = list(string)
}

variable "kubernetes_version" {
  type    = string
  default = "1.30"
}

resource "aws_eks_cluster" "main" {
  name     = "${var.name}-eks"
  role_arn = var.role_arn
  version  = var.kubernetes_version

  vpc_config {
    subnet_ids = var.subnet_ids
  }
}

output "cluster_id" {
  value = aws_eks_cluster.main.id
}

output "endpoint" {
  value = aws_eks_cluster.main.endpoint
}
