# skybridge/azure-observability v0.1.0 — telemetry sink for run-correlated signals.
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

variable "retention_days" {
  type        = number
  description = "Log retention. Cost-aware: keep dev short."
  default     = 30
}

resource "azurerm_log_analytics_workspace" "main" {
  name                = "${var.name}-logs"
  resource_group_name = var.resource_group_name
  location            = var.location
  retention_in_days   = var.retention_days
}

output "workspace_id" {
  value = azurerm_log_analytics_workspace.main.id
}
