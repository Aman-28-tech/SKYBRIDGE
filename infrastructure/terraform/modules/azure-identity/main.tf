# skybridge/azure-identity v0.1.0 — workload identity, federation only.
# No static keys, no client secrets. Least privilege via role assignments
# scoped by the caller (stack level), never subscription-wide here.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "~> 4.0" }
  }
}

variable "name" {
  type = string
}

variable "resource_group_name" {
  type = string
}

variable "location" {
  type = string
}

variable "oidc_issuer_url" {
  type        = string
  description = "AKS OIDC issuer for federated credentials."
}

variable "subject" {
  type        = string
  description = "Federated subject, e.g. system:serviceaccount:<ns>:<sa>."
}

resource "azurerm_user_assigned_identity" "workload" {
  name                = "${var.name}-workload"
  resource_group_name = var.resource_group_name
  location            = var.location
}

resource "azurerm_federated_identity_credential" "workload" {
  name                = "${var.name}-federated"
  resource_group_name = var.resource_group_name
  parent_id           = azurerm_user_assigned_identity.workload.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = var.oidc_issuer_url
  subject             = var.subject
}

output "identity_id" {
  value = azurerm_user_assigned_identity.workload.id
}

output "principal_id" {
  value = azurerm_user_assigned_identity.workload.principal_id
}
