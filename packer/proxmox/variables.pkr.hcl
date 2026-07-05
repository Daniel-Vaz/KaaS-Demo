# Variables for the Proxmox golden-image build (ubuntu-k8s.pkr.hcl). The connection and placement
# values default to the KAAS_PROXMOX_* environment - the same ones the worker uses - so
# `make golden-image-proxmox` works off a sourced .env with nothing else to pass.

variable "proxmox_endpoint" {
  type        = string
  default     = env("KAAS_PROXMOX_ENDPOINT")
  description = "Proxmox API base, e.g. https://172.23.234.12:8006/. Defaults to $KAAS_PROXMOX_ENDPOINT (the plugin's /api2/json is appended)."
}

variable "proxmox_username" {
  type        = string
  default     = env("KAAS_PROXMOX_USERNAME")
  description = "Proxmox user, e.g. kaas@pve (or user@realm!tokenid when using proxmox_token). Defaults to $KAAS_PROXMOX_USERNAME."
}

variable "proxmox_password" {
  type        = string
  default     = env("KAAS_PROXMOX_PASSWORD")
  description = "Proxmox password (password auth). Defaults to $KAAS_PROXMOX_PASSWORD. Ignored when proxmox_token is set."
  sensitive   = true
}

variable "proxmox_token" {
  type        = string
  default     = env("KAAS_PROXMOX_TOKEN")
  description = "API-token secret (token auth). When set, proxmox_username must be user@realm!tokenid. Defaults to $KAAS_PROXMOX_TOKEN."
  sensitive   = true
}

variable "proxmox_insecure" {
  type        = bool
  default     = true
  description = "Accept a self-signed Proxmox certificate (the lab's)."
}

variable "proxmox_node" {
  type        = string
  default     = env("KAAS_PROXMOX_NODE")
  description = "Proxmox node the build VM runs on, e.g. proxmox01. Defaults to $KAAS_PROXMOX_NODE."
}

variable "proxmox_datastore" {
  type        = string
  default     = env("KAAS_PROXMOX_DATASTORE")
  description = "Datastore for the build VM's disk and cloud-init drive, e.g. Pool3ParNew. Defaults to $KAAS_PROXMOX_DATASTORE."
}

variable "proxmox_bridge" {
  type        = string
  default     = env("KAAS_PROXMOX_NET_BRIDGE")
  description = "Bridge the build VM attaches to, e.g. vmbr0. Defaults to $KAAS_PROXMOX_NET_BRIDGE."
}

variable "proxmox_vlan" {
  type        = string
  default     = env("KAAS_PROXMOX_NET_VLAN")
  description = "VLAN tag for the build VM's NIC on a VLAN-aware bridge (empty/0 = untagged). Defaults to $KAAS_PROXMOX_NET_VLAN."
}

variable "seed_template" {
  type        = string
  default     = "ubuntu-26.04-cloudimg-seed"
  description = "The stock Ubuntu cloud-image TEMPLATE to clone from (created once with `qm`; see the build file's header). It must have a cloud-init drive. Must match os_name."
}

variable "ssh_username" {
  type        = string
  default     = "ubuntu"
  description = "The cloud-image's default cloud-init user, which the Proxmox plugin adds the build SSH key to. Stock Ubuntu cloud images use `ubuntu`."
}

# --- Build-VM networking. On a DHCP node network the defaults just work. On a STATIC-only network
# (KAAS_PROXMOX_NET_MODE=static - no DHCP) the build VM cannot get an address, so set build_ip to a
# free CIDR address plus a gateway, e.g. KAAS_PROXMOX_BUILD_IP=172.23.252.240/24. The build VM also
# needs DNS to apt-get, taken from KAAS_PROXMOX_NET_DNS by default.
variable "build_ip" {
  type        = string
  default     = env("KAAS_PROXMOX_BUILD_IP")
  description = "Build VM address: a free `<ip>/<prefix>` on the node network, or empty/'dhcp' for DHCP. Defaults to $KAAS_PROXMOX_BUILD_IP."
}

variable "build_gateway" {
  type        = string
  default     = env("KAAS_PROXMOX_NET_GATEWAY")
  description = "Gateway for a static build_ip. Defaults to $KAAS_PROXMOX_NET_GATEWAY."
}

variable "build_nameserver" {
  type        = string
  default     = env("KAAS_PROXMOX_NET_DNS")
  description = "DNS resolver(s) for the build VM (comma- or space-separated). Defaults to $KAAS_PROXMOX_NET_DNS."
}

variable "k8s_version" {
  type        = string
  default     = "1.36.2"
  description = "Kubernetes version to bake (should match a catalog bundle's kubernetes)."
}

variable "os_name" {
  type        = string
  default     = "ubuntu-26.04"
  description = "Catalog OS name this image is for (should match a catalog os[].name)."
}

variable "output_name" {
  type        = string
  default     = "ubuntu-26.04-k8s-1.36.2"
  description = "Template name. Convention (catalog.GoldenImageNameFor for proxmox): <os_name>-k8s-<k8s_version>, no file suffix. `make golden-image-proxmox` computes it from OS_NAME/K8S_VERSION."
}
