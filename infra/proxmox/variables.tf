variable "node_name" {
  description = "Proxmox node the cluster's VMs run on, e.g. proxmox01"
  type        = string
}

variable "datastore" {
  description = "Datastore for the VMs' disks (and the cloud-init drive), e.g. Pool3ParNew"
  type        = string
}

variable "bridge" {
  description = "Linux/OVS bridge the nodes attach to, e.g. vmbr0"
  type        = string
}

variable "vlan" {
  description = "VLAN tag for the node NIC on a VLAN-aware bridge (0 = untagged)"
  type        = number
  default     = 0
}

variable "cluster_id" {
  description = "Cluster id - namespaces VM names and seeds the deterministic per-node MACs"
  type        = string
}

variable "cluster_name" {
  description = "Human cluster name (for VM descriptions)"
  type        = string
}

variable "ip_mode" {
  description = <<-EOT
    How node addresses are assigned:
      dhcp   - the site's DHCP server does it; the module reads the result back from the QEMU guest
               agent (ipv4_addresses). nodes[].ip must be empty.
      static - the platform allocated them (internal/netpool); the module injects the address via
               the native cloud-init ip_config. nodes[].ip must be set for every node.
  EOT
  type        = string
  validation {
    condition     = contains(["dhcp", "static"], var.ip_mode)
    error_message = "ip_mode must be \"dhcp\" or \"static\"."
  }
}

variable "network_cidr" {
  description = "The bridge's subnet, e.g. 172.23.234.0/24 (static mode: the nodes' prefix length)"
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
  description = "Public key injected via cloud-init (the kaas user), so Ansible can reach the nodes"
  type        = string
}

variable "nodes" {
  description = <<-EOT
    The cluster's VMs. `image` is a Proxmox TEMPLATE name (catalog.GoldenImageNameFor for proxmox,
    e.g. "ubuntu-24.04-k8s-1.36.2") that must exist on node_name; `ip` is the pre-allocated address
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
    # domain.NodeDisk). ORDER IS SIGNIFICANT: the index fixes each disk's SCSI slot, so the platform
    # always sends them in a stable (name) order. `wwn` is the platform-minted identity - on Proxmox
    # we set it as the disk's SERIAL, which surfaces in-guest at /dev/disk/by-id/scsi-…_<serial>, the
    # token the node_disks role resolves the device by (like the kvm wwn; unlike vsphere, which reads
    # its identity back).
    extra_disks = optional(list(object({
      name    = string
      size_gb = number
      wwn     = string
    })), [])
  }))
}
