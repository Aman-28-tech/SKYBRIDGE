# skybridge/azure-routing v0.1.0 — Front Door Standard with AWS + Azure origins.
# Weighted origin routing implements the read-only canary stages.
# Planning this router performs no cutover by itself.
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

variable "aws_origin_host" {
  type        = string
  description = "External AWS origin hostname."
}

variable "azure_origin_host" {
  type        = string
  description = "Azure origin hostname."
}

variable "aws_weight" {
  type        = number
  description = "Desired AWS origin weight (canary stage)."
  default     = 100
}

variable "azure_weight" {
  type        = number
  description = "Desired Azure origin weight (canary stage)."
  default     = 0
}

resource "azurerm_cdn_frontdoor_profile" "main" {
  name                = "${var.name}-fd"
  resource_group_name = var.resource_group_name
  sku_name            = "Standard_AzureFrontDoor"
}

resource "azurerm_cdn_frontdoor_endpoint" "main" {
  name                     = "${var.name}-endpoint"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.main.id
}

resource "azurerm_cdn_frontdoor_origin_group" "main" {
  name                     = "${var.name}-origins"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.main.id

  health_probe {
    interval_in_seconds = 30
    path                = "/healthz"
    protocol            = "Https"
    request_type        = "GET"
  }

  load_balancing {
  }
}

resource "azurerm_cdn_frontdoor_origin" "aws" {
  name                           = "aws-origin"
  cdn_frontdoor_origin_group_id  = azurerm_cdn_frontdoor_origin_group.main.id
  host_name                      = var.aws_origin_host
  weight                         = var.aws_weight
  enabled                        = true
  certificate_name_check_enabled = true
}

resource "azurerm_cdn_frontdoor_origin" "azure" {
  name                           = "azure-origin"
  cdn_frontdoor_origin_group_id  = azurerm_cdn_frontdoor_origin_group.main.id
  host_name                      = var.azure_origin_host
  weight                         = var.azure_weight
  enabled                        = true
  certificate_name_check_enabled = true
}

output "endpoint_host" {
  value = azurerm_cdn_frontdoor_endpoint.main.host_name
}
