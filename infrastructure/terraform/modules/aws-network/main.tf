# skybridge/aws-network v0.1.0 — source-side VPC reference footprint.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

variable "name" {
  type = string
}

variable "cidr_block" {
  type    = string
  default = "10.1.0.0/16"
}

resource "aws_vpc" "main" {
  cidr_block           = var.cidr_block
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = "${var.name}-vpc" }
}

resource "aws_subnet" "workload" {
  vpc_id     = aws_vpc.main.id
  cidr_block = cidrsubnet(var.cidr_block, 8, 1)
  tags       = { Name = "${var.name}-workload" }
}

resource "aws_subnet" "data" {
  vpc_id     = aws_vpc.main.id
  cidr_block = cidrsubnet(var.cidr_block, 8, 2)
  tags       = { Name = "${var.name}-data" }
}

resource "aws_security_group" "default_deny" {
  name        = "${var.name}-default-deny"
  description = "Default deny with explicit workload paths."
  vpc_id      = aws_vpc.main.id
}

output "vpc_id" {
  value = aws_vpc.main.id
}

output "subnet_ids" {
  value = {
    workload = aws_subnet.workload.id
    data     = aws_subnet.data.id
  }
}
