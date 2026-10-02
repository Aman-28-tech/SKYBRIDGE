# skybridge/azure-database v0.1.0 — private Azure Database for PostgreSQL.
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

variable "postgres_version" {
  type        = string
  description = "Mirrors plan database.major_version."
}

variable "storage_mb" {
  type        = number
  description = "Mirrors plan database.storage_gib converted to MB."
}

variable "extensions" {
  type        = list(string)
  description = "Mirrors plan database.extensions."
  default     = []
}

variable "delegated_subnet_id" {
  type        = string
  description = "Data subnet for private access."
}

variable "private_dns_zone_id" {
  type        = string
  description = "Private DNS zone for the server FQDN."
}

variable "admin_login" {
  type        = string
  description = "Admin username. Password comes from Key Vault at apply time, never committed."
}

variable "admin_password" {
  type        = string
  description = "Admin password. Supply via TF_VAR_admin_password from Key Vault at apply time. Never committed, never logged."
  sensitive   = true
}

variable "sku_name" {
  type    = string
  default = "GP_Standard_D2s_v3"
}

resource "azurerm_postgresql_flexible_server" "main" {
  name                          = "${var.name}-pg"
  resource_group_name           = var.resource_group_name
  location                      = var.location
  version                       = var.postgres_version
  storage_mb                    = var.storage_mb
  sku_name                      = var.sku_name
  delegated_subnet_id           = var.delegated_subnet_id
  private_dns_zone_id           = var.private_dns_zone_id
  public_network_access_enabled = false

  administrator_login    = var.admin_login
  administrator_password = var.admin_password
}

resource "azurerm_postgresql_flexible_server_configuration" "wal_level" {
  name      = "wal_level"
  server_id = azurerm_postgresql_flexible_server.main.id
  value     = "logical"
}

resource "azurerm_postgresql_flexible_server_database" "cloudshop" {
  name      = "cloudshop"
  server_id = azurerm_postgresql_flexible_server.main.id
  charset   = "UTF8"
}

resource "azurerm_postgresql_flexible_server_configuration" "extensions" {
  for_each  = toset(var.extensions)
  name      = "azure.extensions"
  server_id = azurerm_postgresql_flexible_server.main.id
  value     = join(",", var.extensions)
}

output "server_id" {
  value = azurerm_postgresql_flexible_server.main.id
}

output "fqdn" {
  value = azurerm_postgresql_flexible_server.main.fqdn
}
