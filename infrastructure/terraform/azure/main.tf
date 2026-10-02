# SKYBRIDGE Azure target root — module composition for one workload environment.
# Deterministic: fixed module set mirroring plan components; per-component
# enable_* flags come from the plan (required flags; blocked refuses upstream).
# Backends via -backend-config (never committed). No apply from this file
# alone: see docs/TERRAFORM_IAC.md protected-apply boundary.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "~> 4.0" }
  }
  backend "azurerm" {
    # storage_account_name/container_name/key via -backend-config; Entra auth (no SAS default)
    container_name   = "tfstate"
    use_azuread_auth = true
  }
}

provider "azurerm" {
  features {}
}

variable "environment" {
  type    = string
  default = "dev"
}

variable "workload_name" {
  type    = string
  default = "cloudshop"
}

variable "location" {
  type    = string
  default = "westeurope"
}

variable "resource_group_name" {
  type = string
}

variable "enable_object_storage" {
  type        = bool
  description = "From plan object-storage.required."
  default     = true
}

variable "postgres_version" {
  type    = string
  default = "17"
}

variable "postgres_storage_gb" {
  type    = number
  default = 32
}

variable "postgres_extensions" {
  type    = list(string)
  default = []
}

variable "object_versioning_required" {
  type    = bool
  default = true
}

variable "node_count" {
  type    = number
  default = 2
}

variable "tenant_id" {
  type = string
}

variable "aws_origin_host" {
  type = string
}

variable "azure_origin_host" {
  type = string
}

variable "db_admin_login" {
  type = string
}

variable "db_admin_password" {
  type        = string
  description = "From Key Vault via TF_VAR_db_admin_password at apply time. Never committed."
  sensitive   = true
}

resource "azurerm_resource_group" "main" {
  name     = "${var.workload_name}-${var.environment}"
  location = var.location
}

module "network" {
  source              = "../modules/azure-network"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
}

module "compute" {
  source              = "../modules/azure-compute"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
  node_count          = var.node_count
  subnet_id           = module.network.subnet_ids["workload"]
}

module "database" {
  source              = "../modules/azure-database"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
  postgres_version    = var.postgres_version
  storage_mb          = var.postgres_storage_gb * 1024
  extensions          = var.postgres_extensions
  delegated_subnet_id = module.network.subnet_ids["data"]
  private_dns_zone_id = azurerm_private_dns_zone.postgres.id
  admin_login         = var.db_admin_login
  admin_password      = var.db_admin_password
}

resource "azurerm_private_dns_zone" "postgres" {
  name                = "privatelink.postgres.database.azure.com"
  resource_group_name = azurerm_resource_group.main.name
}

module "cache" {
  source              = "../modules/azure-cache"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
}

module "object" {
  source = "../modules/azure-object"
  count  = var.enable_object_storage ? 1 : 0

  name                = "${var.workload_name}${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
  versioning_required = var.object_versioning_required
}

module "queue" {
  source              = "../modules/azure-queue"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
}

module "identity" {
  source              = "../modules/azure-identity"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
  oidc_issuer_url     = module.compute.oidc_issuer_url
  subject             = "system:serviceaccount:cloudshop:workload"
}

module "routing" {
  source              = "../modules/azure-routing"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  aws_origin_host     = var.aws_origin_host
  azure_origin_host   = var.azure_origin_host
}

module "observability" {
  source              = "../modules/azure-observability"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
}

module "security" {
  source              = "../modules/azure-security"
  name                = "${var.workload_name}-${var.environment}"
  resource_group_name = azurerm_resource_group.main.name
  location            = var.location
  tenant_id           = var.tenant_id
}
