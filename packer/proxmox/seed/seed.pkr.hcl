# Rebakes the Proxmox cloud-image SEED template so it boots with net.ifnames=0.
#
# Proxmox's native cloud-init writes a netplan keyed on the name eth0 (match: macaddress +
# set-name: eth0). Ubuntu's predictable naming brings the NIC up as ens18 first and DHCP takes a
# lease immediately, so the rename to eth0 fails "[busy]" and a STATIC address is bound to an
# interface that never exists - the VM is stranded on its DHCP lease and unreachable at the address
# it was given. The golden-image build fixes this for cluster nodes, but only from the moment it
# runs, so the BUILD VM itself (which is static when KAAS_PROXMOX_BUILD_IP is set) strands itself
# before Ansible ever starts. Putting net.ifnames=0 in the seed fixes it for every clone from boot.
#
# Clones the current seed, sets the kernel cmdline, re-generalizes cloud-init, and leaves a template.
packer {
  required_plugins {
    proxmox = {
      source  = "github.com/hashicorp/proxmox"
      version = ">= 1.1.3"
    }
  }
}

variable "proxmox_endpoint" {
  type = string
  default = env("KAAS_PROXMOX_ENDPOINT")
}
variable "proxmox_username" {
  type = string
  default = env("KAAS_PROXMOX_USERNAME")
}
variable "proxmox_password" {
  type = string
  default = env("KAAS_PROXMOX_PASSWORD")
  sensitive = true
}
variable "proxmox_node" {
  type = string
  default = env("KAAS_PROXMOX_NODE")
}
variable "proxmox_datastore" {
  type = string
  default = env("KAAS_PROXMOX_DATASTORE")
}
variable "proxmox_bridge" {
  type = string
  default = env("KAAS_PROXMOX_NET_BRIDGE")
}
variable "proxmox_vlan" {
  type = string
  default = env("KAAS_PROXMOX_NET_VLAN")
}
variable "nameserver" {
  type = string
  default = env("KAAS_PROXMOX_NET_DNS")
}
variable "seed_template" {
  type = string
  default = "ubuntu-26.04-cloudimg-seed"
}
variable "output_name" {
  type    = string
  default = "ubuntu-26.04-cloudimg-seed-ifnames"
}

locals {
  api_url = endswith(var.proxmox_endpoint, "/api2/json") ? var.proxmox_endpoint : "${trimsuffix(var.proxmox_endpoint, "/")}/api2/json"
  # Same normalisations the golden-image template does: Proxmox wants space-separated nameservers
  # (the env var is comma-separated) and a numeric VLAN tag (0 = untagged).
  nameserver = trimspace(replace(var.nameserver, ",", " "))
  vlan_tag   = var.proxmox_vlan == "" ? 0 : parseint(var.proxmox_vlan, 10)
}

source "proxmox-clone" "seed" {
  proxmox_url              = local.api_url
  username                 = var.proxmox_username
  password                 = var.proxmox_password
  insecure_skip_tls_verify = true
  node                     = var.proxmox_node

  clone_vm     = var.seed_template
  full_clone   = true
  task_timeout = "10m"

  vm_name              = var.output_name
  template_name        = var.output_name
  template_description = "Ubuntu cloud image seed + net.ifnames=0 (KaaS build prerequisite)"
  scsi_controller      = "virtio-scsi-single"

  cores  = 2
  memory = 2048

  cloud_init              = true
  cloud_init_storage_pool = var.proxmox_datastore
  qemu_agent              = true
  nameserver              = local.nameserver == "" ? null : local.nameserver
  network_adapters {
    bridge   = var.proxmox_bridge
    model    = "virtio"
    vlan_tag = local.vlan_tag
  }
  # DHCP deliberately: a static build VM cannot come up until the very fix this build applies.
  ipconfig {
    ip = "dhcp"
  }

  communicator            = "ssh"
  ssh_username            = "ubuntu"
  ssh_timeout             = "20m"
  ssh_keep_alive_interval = "10s"
  ssh_read_write_timeout  = "3m"
}

build {
  name    = "proxmox-seed-ifnames"
  sources = ["source.proxmox-clone.seed"]

  # Everything in ONE provisioner that starts the moment SSH is up, and NO waiting on cloud-init:
  # roughly two minutes after boot cloud-init renames the NIC (ens18 -> eth0, per the netplan Proxmox
  # generates) and the session dies with it - which is the whole reason this seed needs fixing. So do
  # the work inside that window, stop cloud-init before cleaning up after it, and get out.
  provisioner "shell" {
    inline = [
      "echo '--- interfaces:'; ip -br addr",
      "echo '--- netplan Proxmox generated:'; sudo cat /etc/netplan/*.yaml 2>/dev/null || true",
      "echo '--- applying net.ifnames=0'",
      "echo 'GRUB_CMDLINE_LINUX_DEFAULT=\"$GRUB_CMDLINE_LINUX_DEFAULT net.ifnames=0 biosdevname=0\"' | sudo tee /etc/default/grub.d/99-kaas-ifnames.cfg",
      "sudo update-grub 2>&1 | tail -3",
      "echo '--- verifying it landed in grub.cfg:'; sudo grep -c 'net.ifnames=0' /boot/grub/grub.cfg || true",
      "echo '--- qemu-guest-agent:'; systemctl is-enabled qemu-guest-agent 2>&1 | head -1; dpkg -l qemu-guest-agent 2>/dev/null | tail -1",
      "echo '--- generalizing'",
      "sudo systemctl stop cloud-init cloud-config cloud-final cloud-init-local 2>/dev/null || true",
      "sudo cloud-init clean --logs",
      "sudo rm -rf /var/lib/cloud/instances /var/lib/cloud/instance",
      "sudo rm -f /etc/ssh/ssh_host_*",
      "sudo truncate -s 0 /etc/machine-id",
      "sudo rm -f /var/lib/dbus/machine-id",
      "sudo rm -f /home/ubuntu/.ssh/authorized_keys",
      "echo '--- done'",
    ]
  }
}
