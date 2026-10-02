# skybridge/azure-cache v0.1.0 — cache-only Redis. Never authoritative:
# loss is a performance event, not data loss (lazy warm, TTL from plan).
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

variable "sku_name" {
  type    = string
  default = "Basic"
}

variable "family" {
  type    = string
  default = "C"
}

variable "capacity" {
  type    = number
  default = 1
}

resource "azurerm_redis_cache" "main" {
  name                = "${var.name}-cache"
  resource_group_name = var.resource_group_name
  location            = var.location
  capacity            = var.capacity
  family              = var.family
  sku_name            = var.sku_name
  minimum_tls_version = "1.2"

  redis_configuration {
  }
}

output "cache_id" {
  value = azurerm_redis_cache.main.id
}

output "hostname" {
  value = azurerm_redis_cache.main.hostname
}
