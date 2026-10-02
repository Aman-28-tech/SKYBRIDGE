# skybridge/azure-queue v0.1.0 — Service Bus with duplicate detection.
# At-least-once delivery + duplicate-safe consumers. No global ordering.
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

variable "sku" {
  type    = string
  default = "Standard"
}

variable "duplicate_detection_minutes" {
  type        = number
  description = "Duplicate-detection window. Mirrors duplicate-safe consumer requirement."
  default     = 10
}

resource "azurerm_servicebus_namespace" "main" {
  name                = "${var.name}-sb"
  resource_group_name = var.resource_group_name
  location            = var.location
  sku                 = var.sku
}

resource "azurerm_servicebus_queue" "jobs" {
  name                                    = "jobs"
  namespace_id                            = azurerm_servicebus_namespace.main.id
  requires_duplicate_detection            = true
  duplicate_detection_history_time_window = "PT${var.duplicate_detection_minutes}M"
}

output "namespace_id" {
  value = azurerm_servicebus_namespace.main.id
}

output "queue_id" {
  value = azurerm_servicebus_queue.jobs.id
}
