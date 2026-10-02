# skybridge/azure-object v0.1.0 — Blob storage with logical-key identity.
# versioning_enabled mirrors plan object-storage.versioning_required.
# Provider version IDs stay out of cross-cloud identity either way.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "~> 4.0" }
  }
}

variable "name" {
  type        = string
  description = "Lowercase alphanumeric storage account prefix."
}

variable "resource_group_name" {
  type = string
}

variable "location" {
  type = string
}

variable "versioning_required" {
  type        = bool
  description = "Mirrors plan object-storage.versioning_required."
}

variable "container_name" {
  type    = string
  default = "cloudshop"
}

resource "azurerm_storage_account" "main" {
  name                     = "${var.name}store"
  resource_group_name      = var.resource_group_name
  location                 = var.location
  account_tier             = "Standard"
  account_replication_type = "LRS"
  min_tls_version          = "TLS1_2"
  blob_properties {
    versioning_enabled = var.versioning_required
  }
}

resource "azurerm_storage_container" "main" {
  name                  = var.container_name
  storage_account_name  = azurerm_storage_account.main.name
  container_access_type = "private"
}

output "account_id" {
  value = azurerm_storage_account.main.id
}

output "container_id" {
  value = azurerm_storage_container.main.id
}
