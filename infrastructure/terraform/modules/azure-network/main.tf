# skybridge/azure-network v0.1.0 — private VNet with workload/data/endpoint subnets.
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "~> 4.0" }
  }
}

variable "name" {
  type        = string
  description = "Base name for network resources."
}

variable "resource_group_name" {
  type        = string
  description = "Resource group hosting the VNet."
}

variable "location" {
  type        = string
  description = "Azure region."
}

variable "address_space" {
  type        = list(string)
  description = "VNet address space. No public exposure is configured here."
  default     = ["10.0.0.0/16"]
}

resource "azurerm_virtual_network" "main" {
  name                = "${var.name}-vnet"
  resource_group_name = var.resource_group_name
  location            = var.location
  address_space       = var.address_space
}

resource "azurerm_subnet" "workload" {
  name                 = "${var.name}-workload"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = [cidrsubnet(var.address_space[0], 8, 1)]
}

resource "azurerm_subnet" "data" {
  name                 = "${var.name}-data"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = [cidrsubnet(var.address_space[0], 8, 2)]
  service_endpoints    = ["Microsoft.Storage", "Microsoft.Sql"]
}

resource "azurerm_subnet" "endpoints" {
  name                 = "${var.name}-endpoints"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = [cidrsubnet(var.address_space[0], 8, 3)]
}

resource "azurerm_network_security_group" "default_deny" {
  name                = "${var.name}-default-deny"
  resource_group_name = var.resource_group_name
  location            = var.location
}

resource "azurerm_network_security_rule" "deny_inbound" {
  name                        = "deny-all-inbound"
  priority                    = 4096
  direction                   = "Inbound"
  access                      = "Deny"
  protocol                    = "*"
  source_port_range           = "*"
  destination_port_range      = "*"
  source_address_prefix       = "*"
  destination_address_prefix  = "*"
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.default_deny.name
}

output "vnet_id" {
  value = azurerm_virtual_network.main.id
}

output "subnet_ids" {
  value = {
    workload  = azurerm_subnet.workload.id
    data      = azurerm_subnet.data.id
    endpoints = azurerm_subnet.endpoints.id
  }
}

output "nsg_id" {
  value = azurerm_network_security_group.default_deny.id
}
