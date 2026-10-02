# skybridge/azure-compute v0.1.0 — AKS cluster for the containerized workload.
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

variable "node_count" {
  type        = number
  description = "Default node-pool size. Mirrors plan compute.replicas."
}

variable "vm_size" {
  type        = string
  description = "Node VM size. Chosen at provisioning from cpu/memory requirements."
  default     = "Standard_D2s_v5"
}

variable "zones" {
  type        = list(string)
  description = "Availability zones. Mirrors plan min_zones."
  default     = ["1", "2", "3"]
}

variable "subnet_id" {
  type        = string
  description = "Workload subnet for nodes (private data plane)."
}

resource "azurerm_kubernetes_cluster" "main" {
  name                = "${var.name}-aks"
  resource_group_name = var.resource_group_name
  location            = var.location
  dns_prefix          = "${var.name}-aks"

  default_node_pool {
    name           = "system"
    node_count     = var.node_count
    vm_size        = var.vm_size
    zones          = var.zones
    vnet_subnet_id = var.subnet_id
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin = "azure"
  }
}

output "cluster_id" {
  value = azurerm_kubernetes_cluster.main.id
}

output "kubelet_identity" {
  value = azurerm_kubernetes_cluster.main.kubelet_identity[0].object_id
}

output "oidc_issuer_url" {
  value = azurerm_kubernetes_cluster.main.oidc_issuer_url
}
