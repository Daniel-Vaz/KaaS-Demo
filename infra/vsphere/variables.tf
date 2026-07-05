variable "datacenter" {
  description = "vSphere datacenter name, e.g. MyDC"
  type        = string
}

variable "compute_cluster" {
  description = "vSphere compute cluster the VMs run on, e.g. CLUSTER01"
  type        = string
}

variable "datastore" {
  description = "Datastore for the VMs' disks, e.g. datastorenl"
  type        = string
}

variable "network" {
  description = "Portgroup the nodes attach to, e.g. serviceVMNetwork"
  type        = string
}

variable "parent_folder" {
  description = "VM folder holding the golden-image templates and the per-cluster folders, e.g. DVaz"
  type        = string
}

variable "folder_name" {
  description = "Per-cluster folder created under parent_folder: \"<cluster-name>-<cluster-id>\""
  type        = string
}

variable "cluster_id" {
  description = "Cluster id - namespaces VM names and seeds the deterministic per-node MACs"
  type        = string
}

variable "cluster_name" {
  description = "Human cluster name (for VM annotations)"
  type        = string
}

variable "ip_mode" {
  description = <<-EOT
    How node addresses are assigned:
      dhcp   - the portgroup's own DHCP server does it; the module reads the result back from
               open-vm-tools. nodes[].ip must be empty.
      static - the platform allocated them (internal/netpool); the module injects the address via
               cloud-init guestinfo. nodes[].ip must be set for every node.
  EOT
  type        = string
  validation {
    condition     = contains(["dhcp", "static"], var.ip_mode)
    error_message = "ip_mode must be \"dhcp\" or \"static\"."
  }
}

variable "network_cidr" {
  description = "The portgroup's subnet, e.g. 172.23.252.0/24 (static mode: the nodes' prefix length)"
  type        = string
}

variable "gateway" {
  description = "Default gateway (static mode only)"
  type        = string
  default     = ""
}

variable "dns" {
  description = "Resolvers (static mode only)"
  type        = list(string)
  default     = []
}

variable "ssh_authorized_key" {
  description = "Public key injected via cloud-init, so Ansible can reach the nodes"
  type        = string
}

variable "nodes" {
  description = <<-EOT
    The cluster's VMs. `image` is a vCenter VM TEMPLATE name (catalog.GoldenImageNameFor, e.g.
    "ubuntu-24.04-k8s-1.36.2") that must exist in parent_folder; `ip` is the pre-allocated address
    in static mode and empty in dhcp mode.
  EOT
  type = list(object({
    name    = string
    role    = string
    cpus    = number
    mem_mb  = number
    disk_gb = number
    image   = string
    ip      = string
    # Extra block devices beyond the root disk, formatted and mounted with LVM by Ansible (see
    # domain.NodeDisk). ORDER IS SIGNIFICANT: the index fixes each disk's SCSI unit_number, so the
    # platform always sends them in a stable (name) order. `wwn` is unused here - vCenter mints the
    # disk's identity itself and the module reads it back (see enable_disk_uuid) - but the field is
    # carried so one Go type serves both modules.
    extra_disks = optional(list(object({
      name    = string
      size_gb = number
      wwn     = string
    })), [])
  }))
}
