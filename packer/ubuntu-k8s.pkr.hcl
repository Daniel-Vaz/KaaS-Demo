# Golden image build: boot a base Ubuntu cloud image, bake containerd + kube* via the
# SAME Ansible `common` role used at provision time, pre-pull the control-plane images, then
# generalize so cloud-init re-runs on first boot. Output: a qcow2 the OpenTofu module clones.
#
#   cd packer && /usr/bin/packer init . && /usr/bin/packer build .
#   (PATH may shadow HashiCorp packer with cracklib's /usr/sbin/packer - use /usr/bin/packer.)

packer {
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = "~> 1.1"
    }
    ansible = {
      source  = "github.com/hashicorp/ansible"
      version = "~> 1.1"
    }
  }
}

locals {
  k8s_minor = join(".", slice(split(".", var.k8s_version), 0, 2)) # 1.36.2 -> 1.36
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
}

source "qemu" "ubuntu-k8s" {
  qemu_binary    = var.qemu_binary
  iso_url        = var.base_image_url
  iso_checksum   = "none"
  disk_image     = true
  disk_size      = "20G"
  format         = "qcow2"
  accelerator    = "kvm"
  net_device     = "virtio-net"
  disk_interface = "virtio"
  headless       = true
  cpus           = 2
  memory         = 2048

  # NoCloud seed so cloud-init makes a passwordless-sudo `packer` user for the SSH connection.
  cd_content = {
    "meta-data" = "instance-id: packer\nlocal-hostname: packer-build\n"
    "user-data" = local.user_data
  }
  cd_label = "cidata"

  ssh_username     = "packer"
  ssh_password     = "packer"
  ssh_timeout      = "20m"
  boot_wait        = "5s"
  shutdown_command = "sudo shutdown -P now"

  output_directory = var.output_dir
  vm_name          = var.output_name
}

build {
  name    = "ubuntu-k8s"
  sources = ["source.qemu.ubuntu-k8s"]

  # Bake node software with the SAME `common` role used at provision time, so at
  # cluster-create time that role is a near no-op.
  provisioner "ansible" {
    playbook_file = "${path.root}/../ansible/playbooks/golden-image.yml"
    user          = "packer"
    extra_arguments = [
      "-e", "k8s_version=${var.k8s_version}",
      "-e", "k8s_minor=${local.k8s_minor}",
    ]
    ansible_env_vars = [
      "ANSIBLE_ROLES_PATH=${path.root}/../ansible/roles",
      "ANSIBLE_HOST_KEY_CHECKING=False",
      # Pipe module files over the SSH session instead of scp/sftp - the latter fails through
      # Packer's SSH proxy on modern OpenSSH ("failed to transfer file ... AnsiballZ_setup.py").
      "ANSIBLE_SSH_TRANSFER_METHOD=piped",
    ]
  }

  # Generalize: reset cloud-init + machine identity so our per-node datasource (SSH key +
  # hostname) applies at first boot, and the image isn't tied to this build VM.
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
