# vSphere golden image: clone a stock Ubuntu cloud seed template, bake containerd + kube* with the
# SAME Ansible `common` role used at provision time (and by the KVM golden image), generalize, and
# mark the result as a VM TEMPLATE the OpenTofu vSphere module clones per node.
#
# The output is a template named "<os_name>-k8s-<k8s_version>" in the parent folder - exactly what
# catalog.GoldenImageNameFor("vsphere", …) produces, and what internal/provision/vsphere preflights
# before a rolling OS upgrade.
#
#   make golden-image-vsphere OS_NAME=ubuntu-26.04 K8S_VERSION=1.36.2
#
# PREREQUISITE - the seed template (once per OS, out of band). See docs/infrastructure.md for the
# exact `govc import.ova` invocation: it needs a datastore, a resource pool, and a NetworkMapping
# (the OVA declares a NIC on "VM Network", which won't exist in your vCenter).
#
# Use the OVA (not the .img): it ships open-vm-tools, which is what lets vCenter report a node's
# DHCP address back to OpenTofu and what makes cloud-init pick the VMware datasource. Never boot
# the seed directly - its cloud-init must still be unrun, or the guestinfo user-data below (which
# creates the `packer` user this build connects as) is ignored. Marking it as a template makes
# booting it by accident impossible.

packer {
  required_plugins {
    vsphere = {
      source  = "github.com/hashicorp/vsphere"
      version = "~> 1.4"
    }
    ansible = {
      source  = "github.com/hashicorp/ansible"
      version = "~> 1.1"
    }
  }
}

locals {
  k8s_minor = join(".", slice(split(".", var.k8s_version), 0, 2)) # 1.36.2 -> 1.36
  # Same NoCloud user-data as the KVM build, delivered over guestinfo instead of a CD.
  user_data = <<-EOT
    #cloud-config
    users:
      - name: packer
        groups: sudo
        sudo: 'ALL=(ALL) NOPASSWD:ALL'
        shell: /bin/bash
        lock_passwd: false
        plain_text_passwd: packer
    ssh_pwauth: true
  EOT
  metadata  = <<-EOT
    instance-id: packer-build
    local-hostname: packer-build
  EOT
}

source "vsphere-clone" "ubuntu-k8s" {
  vcenter_server      = var.vsphere_server
  username            = var.vsphere_username
  password            = var.vsphere_password
  insecure_connection = var.vsphere_insecure

  datacenter = var.vsphere_datacenter
  cluster    = var.vsphere_cluster
  datastore  = var.vsphere_datastore
  folder     = var.vsphere_folder

  template = var.seed_template
  vm_name  = var.output_name

  CPUs   = 2
  RAM    = 2048
  linked_clone = false

  # cloud-init through guestinfo - the same mechanism the provisioning module uses, so the golden
  # image is exercised the way it will be used.
  configuration_parameters = {
    "guestinfo.metadata"          = base64encode(local.metadata)
    "guestinfo.metadata.encoding" = "base64"
    "guestinfo.userdata"          = base64encode(local.user_data)
    "guestinfo.userdata.encoding" = "base64"
  }

  communicator     = "ssh"
  ssh_username     = "packer"
  ssh_password     = "packer"
  ssh_timeout      = "20m"
  shutdown_command = "sudo shutdown -P now"

  convert_to_template = true
}

build {
  name    = "vsphere-ubuntu-k8s"
  sources = ["source.vsphere-clone.ubuntu-k8s"]

  # open-vm-tools is how vCenter learns a node's IP (the module's dhcp mode reads it back as the
  # node's address) and how cloud-init detects the VMware datasource. The OVA ships it; install it
  # explicitly so a seed built some other way still yields a working golden image.
  provisioner "shell" {
    inline = [
      "sudo cloud-init status --wait || true",
      "sudo apt-get update",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y open-vm-tools",
    ]
  }

  # Bake node software with the SAME `common` role used at provision time (and by the KVM golden
  # image), so at cluster-create time that role is a near no-op and the two providers converge on
  # identical nodes.
  provisioner "ansible" {
    playbook_file = "${path.root}/../../ansible/playbooks/golden-image.yml"
    user          = "packer"
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

  # Generalize: reset cloud-init + machine identity so each cloned node applies its OWN guestinfo
  # (hostname, SSH key, and in static mode its address) at first boot.
  provisioner "shell" {
    inline = [
      "sudo cloud-init clean --logs",
      "sudo rm -rf /var/lib/cloud/instances /var/lib/cloud/instance",
      "sudo rm -f /etc/ssh/ssh_host_*",
      "sudo truncate -s 0 /etc/machine-id",
      "sudo rm -f /var/lib/dbus/machine-id",
      "sudo rm -f /home/packer/.ssh/authorized_keys",
      "sudo apt-get clean",
      "sudo rm -rf /var/lib/apt/lists/*",
    ]
  }
}
