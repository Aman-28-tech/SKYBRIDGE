# skybridge/aws-object v0.1.0 — source S3 reference footprint.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

variable "name" {
  type = string
}

variable "versioning_required" {
  type        = bool
  description = "Mirrors plan object-storage.versioning_required."
}

resource "aws_s3_bucket" "main" {
  bucket = "${var.name}-objects"
}

resource "aws_s3_bucket_versioning" "main" {
  bucket = aws_s3_bucket.main.id
  versioning_configuration {
    status = var.versioning_required ? "Enabled" : "Suspended"
  }
}

resource "aws_s3_bucket_public_access_block" "main" {
  bucket                  = aws_s3_bucket.main.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

output "bucket_id" {
  value = aws_s3_bucket.main.id
}
