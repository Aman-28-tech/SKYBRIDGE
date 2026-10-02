# skybridge/aws-database v0.1.0 — source RDS PostgreSQL reference footprint.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

variable "name" {
  type = string
}

variable "engine_version" {
  type        = string
  description = "Mirrors plan database.major_version."
}

variable "storage_gb" {
  type        = number
  description = "Mirrors plan database.storage_gib."
}

variable "subnet_ids" {
  type = list(string)
}

variable "username" {
  type = string
}

variable "instance_class" {
  type    = string
  default = "db.t4g.micro"
}

resource "aws_db_subnet_group" "main" {
  name       = "${var.name}-db-subnets"
  subnet_ids = var.subnet_ids
}

resource "aws_db_instance" "main" {
  identifier                  = "${var.name}-pg"
  engine                      = "postgres"
  engine_version              = var.engine_version
  instance_class              = var.instance_class
  allocated_storage           = var.storage_gb
  db_subnet_group_name        = aws_db_subnet_group.main.name
  username                    = var.username
  manage_master_user_password = true
  publicly_accessible         = false
  backup_retention_period     = 7
  skip_final_snapshot         = true
}

output "db_id" {
  value = aws_db_instance.main.id
}

output "endpoint" {
  value = aws_db_instance.main.endpoint
}
