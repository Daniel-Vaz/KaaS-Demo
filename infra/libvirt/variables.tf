variable "cluster_name" {
  type        = string
  description = "Unique per-cluster prefix for libvirt object names."
}

variable "libvirt_uri" {
  type    = string
  default = "qemu:///system"
}

variable "pool" {
  type    = string
  default = "default"
}

variable "network_cidr" {
  type        = string
  description = "CIDR for this cluster's dedicated, isolated libvirt network (e.g. 10.200.3.0/24). Its VMs draw addresses from this subnet's DHCP; the HA API VIP is a high host in it."
}

variable "network_mode" {
  type        = string
  default     = "nat"
  description = "libvirt forwarding mode for the per-cluster network. 'nat' keeps clusters L2-isolated from each other while preserving host reachability (for Ansible/kubectl) and egress (for Helm/image pulls)."
}

variable "base_image" {
  type        = string
  description = "Fallback base/golden qcow2 for any node with no per-node image. A path/URL where OpenTofu runs when preloaded_images is false; a path to a volume already in `pool` on the hypervisor when it is true."
}

variable "preloaded_images" {
  type        = bool
  default     = false
  description = "When true, `base_image`/`nodes[].image` are paths to volumes that ALREADY exist in `pool` ON THE HYPERVISOR (staged out-of-band by the provisioner, which reports back where it put them) and node root volumes back onto them directly - nothing is uploaded through OpenTofu. Set for a remote KVM host, where the provider's per-cluster import would stream a multi-GB image over the libvirt connection every time. When false (local libvirt), the paths are local to the machine running OpenTofu and the module imports each image itself."
}

variable "ssh_authorized_key" {
  type        = string
  description = "SSH public key injected via cloud-init so Ansible can reach the nodes."
}

variable "nodes" {
  description = "The VMs to create for this cluster."
  type = list(object({
    name    = string
    role    = string
    cpus    = number
    mem_mb  = number
    disk_gb = number
    # Per-node golden image (path/URL). Lets a single node be re-cloned onto a new-OS/Kubernetes
    # image during a rolling upgrade. Empty falls back to base_image (the single-image default).
    image = optional(string, "")
    # Extra block devices beyond the root disk, formatted and mounted with LVM by Ansible (see
    # domain.NodeDisk, ansible/roles/node_disks). `wwn` is the platform-minted stable identity the
    # guest sees at /dev/disk/by-id/wwn-<wwn>; it must be exactly 16 hex digits (libvirt rejects
    # anything else) and is what makes "format the RIGHT disk" safe - guest device names shift.
    extra_disks = optional(list(object({
      name    = string
      size_gb = number
      wwn     = string
    })), [])
  }))
}
