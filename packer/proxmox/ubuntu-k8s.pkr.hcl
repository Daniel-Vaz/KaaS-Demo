# Proxmox golden image: clone a stock Ubuntu cloud-image seed template, bake containerd + kube* with
# the SAME Ansible `common` role used at provision time (and by the KVM and vSphere golden images),
# ensure qemu-guest-agent, generalize, and leave the result as a Proxmox VM TEMPLATE the OpenTofu
# proxmox module clones per node.
#
# The output is a template named "<os_name>-k8s-<k8s_version>" on the target node - exactly what
# catalog.GoldenImageNameFor("proxmox", …) produces, and what internal/provision/proxmox preflights
# before a rolling OS upgrade.
#
#   make golden-image-proxmox OS_NAME=ubuntu-26.04 K8S_VERSION=1.36.2
#
# PREREQUISITE - the seed template (once per OS, out of band). Create a base Ubuntu cloud-image
# template on the node. Two things are load-bearing and easy to miss: the seed MUST carry
# qemu-guest-agent (Packer's proxmox-clone plugin discovers the build VM's IP via the agent - with no
# agent it never learns where to SSH and hangs until timeout), and its disk MUST be grown past the
# ~3.5 GB stock image (containerd + kube* + pre-pulled images don't fit in 3.5 GB):
#
#   wget https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img
#   # Bake the guest agent into the image (needs libguestfs-tools: apt-get install -y libguestfs-tools)
#   virt-customize -a resolute-server-cloudimg-amd64.img --install qemu-guest-agent
#   qm create 9000 --name ubuntu-26.04-cloudimg-seed --memory 2048 --cores 2 \
#       --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single --agent enabled=1 --ostype l26
#   qm set 9000 --scsi0 Pool3ParNew:0,import-from=$PWD/resolute-server-cloudimg-amd64.img
#   qm disk resize 9000 scsi0 20G          # cloud-init growpart expands the FS on each clone
#   qm set 9000 --ide2 Pool3ParNew:cloudinit --boot order=scsi0 --serial0 socket --vga serial0
#   qm template 9000
#
# Packer reaches the clone over cloud-init: the plugin injects its build SSH key onto the image's
# default user (`ubuntu`) and the network config below, and discovers the clone's IP via the guest
# agent. On a STATIC-only node network (KAAS_PROXMOX_NET_MODE=static) set KAAS_PROXMOX_BUILD_IP to a
# free `<ip>/<prefix>` so the build VM has an address; on a VLAN-aware bridge set KAAS_PROXMOX_NET_VLAN.
# The seed's cloud-init must be unrun - a fresh clone of a stock cloud image is; marking it a template
# makes booting it by accident impossible.

packer {
  required_plugins {
    proxmox = {
      source  = "github.com/hashicorp/proxmox"
      version = "~> 1.2"
    }
    ansible = {
      source  = "github.com/hashicorp/ansible"
      version = "~> 1.1"
    }
  }
}

locals {
  k8s_minor       = join(".", slice(split(".", var.k8s_version), 0, 2)) # 1.36.2 -> 1.36
  proxmox_api_url = "${trimsuffix(var.proxmox_endpoint, "/")}/api2/json"
  # Empty build_ip means DHCP; otherwise it is a static "<ip>/<prefix>" and needs a gateway.
  build_ip     = var.build_ip == "" ? "dhcp" : var.build_ip
  build_static = local.build_ip != "dhcp"
  # Proxmox wants space-separated nameservers; KAAS_PROXMOX_NET_DNS is comma-separated.
  build_nameserver = trimspace(replace(var.build_nameserver, ",", " "))
  # VLAN tag as a number (0 = untagged); the env-sourced var is a string, empty when unset.
  vlan_tag = var.proxmox_vlan == "" ? 0 : parseint(var.proxmox_vlan, 10)
}

source "proxmox-clone" "ubuntu-k8s" {
  proxmox_url              = local.proxmox_api_url
  username                 = var.proxmox_username
  password                 = var.proxmox_token == "" ? var.proxmox_password : null
  token                    = var.proxmox_token == "" ? null : var.proxmox_token
  insecure_skip_tls_verify = var.proxmox_insecure
  node                     = var.proxmox_node

  clone_vm   = var.seed_template
  full_clone = true
  # A full clone of the (now ~20 GB, thick-LVM) seed on shared storage takes minutes - well over the
  # plugin's 1-minute default, which otherwise fails the build with "Wait timeout for qmclone".
  task_timeout = "10m"

  vm_name              = var.output_name
  template_name        = var.output_name
  template_description = "KaaS golden image - ${var.os_name}, Kubernetes ${var.k8s_version}"
  scsi_controller      = "virtio-scsi-single"

  cores  = 2
  memory = 2048

  # Reuse the seed's cloud-init drive: Packer injects a temporary SSH key (added to the image's
  # default user) plus the network config below, so it can reach the clone. In DHCP mode the guest
  # agent reports the address back; in static mode Packer connects to the address it assigned.
  cloud_init              = true
  cloud_init_storage_pool = var.proxmox_datastore
  qemu_agent              = true
  nameserver              = local.build_nameserver == "" ? null : local.build_nameserver
  network_adapters {
    bridge   = var.proxmox_bridge
    model    = "virtio"
    vlan_tag = local.vlan_tag
  }
  ipconfig {
    ip      = local.build_ip
    gateway = local.build_static ? var.build_gateway : null
  }

  communicator = "ssh"
  ssh_username = var.ssh_username
  ssh_timeout  = "20m"
}

build {
  name    = "proxmox-ubuntu-k8s"
  sources = ["source.proxmox-clone.ubuntu-k8s"]

  # qemu-guest-agent is how Proxmox learns a node's IP (the module's dhcp mode reads it back as the
  # node's address), so the final template must have it. Installed here rather than baked into the
  # seed, so a plain stock-cloud-image seed suffices (idempotent).
  #
  # No `systemctl enable`: on Ubuntu qemu-guest-agent.service ships no [Install] section - it is
  # udev-activated when the virtio-serial channel appears (which it does with `agent: enabled=1`), so
  # `enable` only prints a harmless "unit files have no installation config" warning and does nothing.
  provisioner "shell" {
    inline = [
      "sudo cloud-init status --wait || true",
      "sudo apt-get update",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-guest-agent",
    ]
  }

  # Bake node software with the SAME `common` role used at provision time (and by the other golden
  # images), so at cluster-create time that role is a near no-op and the providers converge on
  # identical nodes.
  provisioner "ansible" {
    playbook_file = "${path.root}/../../ansible/playbooks/golden-image.yml"
    user          = var.ssh_username
    extra_arguments = [
      "-e", "k8s_version=${var.k8s_version}",
      "-e", "k8s_minor=${local.k8s_minor}",
    ]
    ansible_env_vars = [
      "ANSIBLE_ROLES_PATH=${path.root}/../../ansible/roles",
      "ANSIBLE_HOST_KEY_CHECKING=False",
      "ANSIBLE_SSH_TRANSFER_METHOD=piped",
    ]
  }

  # Force legacy `eth0` interface naming. Proxmox's native cloud-init generates its network config
  # keyed on the name `eth0` (match: macaddress + set-name: eth0), but Ubuntu's default predictable
  # naming brings the NIC up as `ens18` first; DHCP grabs a lease immediately, and by the time netplan
  # tries to rename ens18 -> eth0 the link is already up, so the rename fails "[busy]". The static
  # address is then bound to an eth0 that never exists and the node is stranded on the DHCP lease,
  # unreachable at its allocated IP - Ansible's "Wait for SSH" hangs forever. With net.ifnames=0 the
  # kernel names the NIC eth0 from boot, so Proxmox's config binds with no rename to race on. (kvm and
  # vsphere are immune: their cloud-init matches purely by MAC and never renames.)
  provisioner "shell" {
    inline = [
      "echo 'GRUB_CMDLINE_LINUX_DEFAULT=\"$GRUB_CMDLINE_LINUX_DEFAULT net.ifnames=0 biosdevname=0\"' | sudo tee /etc/default/grub.d/99-kaas-ifnames.cfg",
      "sudo update-grub",
    ]
  }

  # Generalize: reset cloud-init + machine identity so each cloned node applies its OWN cloud-init
  # (hostname, SSH key, and in static mode its address) at first boot.
  #
  # We deliberately do NOT delete the ssh_username's authorized_keys here (unlike the kvm/vsphere
  # builds): the proxmox-clone plugin injected its temporary BUILD key into it and removes that key
  # itself during teardown (a `sed`/`rm .bak` on the file). Deleting the file first makes that
  # teardown print harmless "authorized_keys: No such file" errors; letting the plugin clean its own
  # key leaves an empty authorized_keys with no build credential - the same clean result, quietly.
  provisioner "shell" {
    inline = [
      "sudo cloud-init clean --logs",
      "sudo rm -rf /var/lib/cloud/instances /var/lib/cloud/instance",
      "sudo rm -f /etc/ssh/ssh_host_*",
      "sudo truncate -s 0 /etc/machine-id",
      "sudo rm -f /var/lib/dbus/machine-id",
      "sudo apt-get clean",
      "sudo rm -rf /var/lib/apt/lists/*",
    ]
  }
}
