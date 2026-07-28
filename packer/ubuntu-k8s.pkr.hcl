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

  # Bake in the QEMU guest agent, the same way the Proxmox image does. The libvirt module wires a
  # virtio-serial `org.qemu.guest_agent.0` channel onto every node, which udev-activates the agent -
  # so nodes come up answering host-side queries (`virsh domifaddr --source agent`) and support
  # agent-driven graceful shutdown / fsfreeze. The node IP is still read from the DHCP lease, not the
  # agent (see infra/libvirt/main.tf's data source), so this is not on the provisioning critical path.
  #
  # No `systemctl enable`: on Ubuntu qemu-guest-agent.service ships no [Install] section - it is
  # udev-activated when the virtio-serial channel appears, so `enable` only warns and does nothing.
  provisioner "shell" {
    inline = [
      "sudo cloud-init status --wait || true",
      "sudo apt-get update",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-guest-agent",
    ]
  }

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

  # Fail the BUILD on a broken initramfs, rather than shipping an image that cannot boot.
  #
  # This is not hypothetical: a build shipped an initramfs truncated to 2.6MB, and every node using
  # it panicked at boot with "Initramfs unpacking failed: ZSTD-compressed data is truncated" ->
  # "VFS: Unable to mount root fs on unknown-block(0,0)". Nothing here regenerates the initramfs on
  # purpose (see the long note in ansible/playbooks/golden-image.yml), but package installs do it
  # via dpkg triggers - cryptsetup and open-iscsi from longhorn_prereqs both add initramfs hooks -
  # so a truncated write is always reachable and is invisible in the finished qcow2.
  #
  # It has to be caught HERE because of how the failure presents downstream: the node has no network
  # to debug over, and the platform reports only OpenTofu's `wait_for_ip` timing out 300 seconds
  # later, which reads like an infrastructure fault rather than a bad image.
  #
  # We VERIFY rather than regenerate, deliberately - `dracut --regenerate-all --force` replaces a
  # working initramfs and is exactly what the golden-image.yml note warns against.
  #
  # The size floor is the check that actually catches truncation: a real Ubuntu initramfs is tens of
  # MB, so 20MB is far below any plausible good value and far above the truncated one. `lsinitrd`
  # (dracut) / `lsinitramfs` (initramfs-tools) walk the whole archive, which also catches corruption
  # that happens to land on a plausible size - but they are secondary, since a bare `zstd -t` cannot
  # be used here: Ubuntu's initrd.img is an uncompressed microcode cpio CONCATENATED with the
  # compressed archive, so testing the file as a single zstd stream fails even when it is healthy.
  provisioner "shell" {
    inline = [
      "set -eu",
      "kver=$(uname -r)",
      "img=/boot/initrd.img-$kver",
      "test -f \"$img\" || { echo \"FATAL: no initramfs at $img\"; exit 1; }",
      "sz=$(stat -c %s \"$img\")",
      "echo \"initramfs $img: $sz bytes\"",
      "if [ \"$sz\" -lt 20000000 ]; then echo \"FATAL: initramfs is $sz bytes - truncated (expected tens of MB)\"; exit 1; fi",
      "if command -v lsinitrd >/dev/null 2>&1; then sudo lsinitrd \"$img\" >/dev/null || { echo 'FATAL: lsinitrd could not read the initramfs'; exit 1; }; elif command -v lsinitramfs >/dev/null 2>&1; then sudo lsinitramfs \"$img\" >/dev/null || { echo 'FATAL: lsinitramfs could not read the initramfs'; exit 1; }; fi",
      "echo 'initramfs OK'",
      # Flush any dpkg trigger still queued, then force everything to disk before Packer powers the
      # VM off - an in-flight initramfs write is the most likely way the truncation happened.
      "sudo dpkg --configure -a",
      "sudo sync",
    ]
  }
}
