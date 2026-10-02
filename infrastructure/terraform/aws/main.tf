# SKYBRIDGE AWS source root — reference footprint composition.
# Mirrors the Azure target shape for describe/verify-side parity.
# Backends via -backend-config (never committed). No apply without the
# protected boundary in docs/TERRAFORM_IAC.md.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
  backend "s3" {
    # bucket/key/region/kms via -backend-config (never committed)
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "environment" {
  type    = string
  default = "dev"
}

variable "workload_name" {
  type    = string
  default = "cloudshop"
}

variable "cluster_role_arn" {
  type = string
}

variable "postgres_version" {
  type    = string
  default = "17"
}

variable "postgres_storage_gb" {
  type    = number
  default = 20
}

variable "db_username" {
  type = string
}

variable "object_versioning_required" {
  type    = bool
  default = true
}

module "network" {
  source = "../modules/aws-network"
  name   = "${var.workload_name}-${var.environment}"
}

module "compute" {
  source     = "../modules/aws-compute"
  name       = "${var.workload_name}-${var.environment}"
  role_arn   = var.cluster_role_arn
  subnet_ids = [module.network.subnet_ids["workload"], module.network.subnet_ids["data"]]
}

module "database" {
  source         = "../modules/aws-database"
  name           = "${var.workload_name}-${var.environment}"
  engine_version = var.postgres_version
  storage_gb     = var.postgres_storage_gb
  subnet_ids     = [module.network.subnet_ids["data"]]
  username       = var.db_username
}

module "object" {
  source              = "../modules/aws-object"
  name                = "${var.workload_name}-${var.environment}"
  versioning_required = var.object_versioning_required
}
