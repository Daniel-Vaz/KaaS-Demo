# One base volume per cluster (from the golden/base image), cloned copy-on-write into a
# per-node volume. cloud-init injects the SSH key + hostname so Ansible can reach each node.
# See docs/networking.md for how the worker reaches these VMs on the libvirt default net.

locals {
  nodes = { for n in var.nodes : n.name => n }
  # Each node clones from its own golden image (per-node OS/Kubernetes), falling back to the single
  # base_image when unset. A node whose image changes (rolling upgrade) re-clones from a different
  # base - replacing just that one VM.
  #
  # Where that base volume COMES FROM depends on preloaded_images:
  #
  #   false (local libvirt): image/base_image are PATHS on the machine running OpenTofu. The provider
  #     imports each distinct one into the pool as a per-cluster COW base volume (libvirt_volume.base
  #     below) by streaming it over the libvirt connection.
  #   true (remote KVM host): image/base_image are VOLUME NAMES already present in `pool`, staged
  #     there once by the provisioner (internal/kvmhost.StageImage). Nodes clone from them by name;
  #     nothing is uploaded through OpenTofu at all. This is not just an optimisation - the provider
  #     hard-codes the SDK's 20m create timeout on a volume, which a multi-GB upload over a slow link
  #     cannot meet, so the import path is simply not viable off-box. See docs/infrastructure.md.
  node_image = { for n in var.nodes : n.name => coalesce(n.image, var.base_image) }
  # Only the import path needs the set of distinct images; preloaded ones already exist in the pool.
  images = var.preloaded_images ? toset([]) : toset(values(local.node_image))
  # Preloaded mode only: the name of the pool volume each node clones from. basename() so a caller
  # that passes a path here still resolves to the staged volume's name.
  node_base_volume = { for k, v in local.node_image : k => basename(v) }
  # A deterministic MAC per node (QEMU's 52:54:00 OUI + 3 bytes derived from cluster+node). Pinning
  # the MAC means a VM re-created for a rolling upgrade reclaims the SAME DHCP lease/IP - which is
  # what lets a single control plane be replaced onto a new OS image without its certs/etcd (both
  # keyed on the node IP) going stale. See internal/config/ansible restore-controlplane.yml.
  node_mac = { for n in var.nodes : n.name => format("52:54:00:%s:%s:%s",
    substr(md5("${var.cluster_name}-${n.name}"), 0, 2),
    substr(md5("${var.cluster_name}-${n.name}"), 2, 2),
    substr(md5("${var.cluster_name}-${n.name}"), 4, 2))
  }

  # Every extra disk across every node, flattened to one map keyed "<node>/<disk>" so each is its own
  # libvirt_volume. Volumes are per-disk (not per-node) because that is the unit that is added and
  # removed independently while the node lives on.
  extra_disks = merge([
    for n in var.nodes : {
      for d in n.extra_disks : "${n.name}/${d.name}" => {
        node    = n.name
        name    = d.name
        size_gb = d.size_gb
        wwn     = d.wwn
      }
    }
  ]...)
}

# One dedicated, isolated network per cluster. A NAT bridge (its own virbrN) gives each cluster
# L2 isolation from every other cluster while still letting the host-networked worker reach the
# nodes (Ansible/kubectl) and the nodes reach the internet (Helm/image pulls). libvirt assigns the
# gateway the first host of the CIDR (.1) and runs DHCP across the subnet; the HA API VIP sits high
# in the range (see internal/netpool). Torn down with the cluster on `tofu destroy`.
resource "libvirt_network" "cluster" {
  name      = "${var.cluster_name}-net"
  mode      = var.network_mode
  addresses = [var.network_cidr]
  autostart = true
  dhcp {
    enabled = true
  }
  dns {
    enabled = true
  }
}

# Import path only (preloaded_images = false): one COW base volume per distinct image, per cluster.
# Empty when the images are already staged in the pool.
resource "libvirt_volume" "base" {
  for_each = local.images
  name     = "${var.cluster_name}-base-${md5(each.value)}.qcow2"
  pool     = var.pool
  source   = each.value
  format   = "qcow2"
}

resource "libvirt_volume" "node" {
  for_each = local.nodes
  name     = "${var.cluster_name}-${each.value.name}.qcow2"
  pool     = var.pool
  # Exactly one of these two is set. try() rather than a conditional on base_volume_id: with
  # preloaded_images the base map is empty, and indexing it would fail even in the untaken branch of
  # a ternary (HCL type-checks both).
  base_volume_id   = try(libvirt_volume.base[local.node_image[each.key]].id, null)
  base_volume_name = var.preloaded_images ? local.node_base_volume[each.key] : null
  base_volume_pool = var.preloaded_images ? var.pool : null
  size             = each.value.disk_gb * 1024 * 1024 * 1024
  format           = "qcow2"
}

# One volume per extra disk. Blank (no base_volume), so the guest sees a raw, unpartitioned device
# that the node_disks Ansible role turns into a PV/VG/LV and mounts.
#
# These are destroyed when the disk leaves var.nodes[].extra_disks - which is exactly why the
# reconciler unmounts and tears the volume group down FIRST (domain.DiskPhaseRemoving): by the time
# this volume disappears, nothing in the guest is using it.
resource "libvirt_volume" "extra" {
  for_each = local.extra_disks
  name     = "${var.cluster_name}-${each.value.node}-${each.value.name}.qcow2"
  pool     = var.pool
  size     = each.value.size_gb * 1024 * 1024 * 1024
  format   = "qcow2"
}

resource "libvirt_cloudinit_disk" "node" {
  for_each  = local.nodes
  name      = "${var.cluster_name}-${each.value.name}-cloudinit.iso"
  pool      = var.pool
  user_data = <<-EOT
    #cloud-config
    hostname: ${each.value.name}
    fqdn: ${each.value.name}
    manage_etc_hosts: true
    users:
      - name: kaas
        sudo: 'ALL=(ALL) NOPASSWD:ALL'
        groups: sudo
        shell: /bin/bash
        ssh_authorized_keys:
          - ${var.ssh_authorized_key}
    ssh_pwauth: false
  EOT
}

resource "libvirt_domain" "node" {
  for_each = local.nodes
  name     = "${var.cluster_name}-${each.value.name}"
  memory   = each.value.mem_mb
  vcpu     = each.value.cpus

  # Expose the real host CPU to the guest. Without this, libvirt/QEMU emulates a baseline `qemu64`
  # model that lacks the x86-64-v2 feature set (SSE4.2, POPCNT, ...), and modern container glibc
  # builds abort on start with "Fatal glibc error: CPU does not support x86-64-v2" - e.g. the kepler
  # add-on crash-loops. host-passthrough is safe here: single host, no live migration.
  cpu {
    mode = "host-passthrough"
  }

  cloudinit = libvirt_cloudinit_disk.node[each.key].id

  # The root disk. MUST stay the first disk block - it is the boot device.
  disk {
    volume_id = libvirt_volume.node[each.key].id
  }

  # Extra disks, on the SCSI bus so each can carry a wwn: libvirt refuses a wwn on virtio
  # ("Only ide and scsi disk support wwn"), and without one the guest has no stable handle on the
  # device - /dev/vdb..vdc are assigned in attach order and renumber when a disk is removed, so a
  # role that formatted "the second disk" would eventually format the wrong one. With scsi+wwn the
  # guest gets /dev/disk/by-id/wwn-0x… , which is what the node_disks role keys on.
  #
  # These blocks only ever take effect when the domain is CREATED (see the lifecycle block below):
  # a fresh node, or one rebuilt by a rolling OS replacement, boots with its full disk set already
  # attached. Disks added to a LIVE node are hot-attached out of band by the platform.
  dynamic "disk" {
    for_each = { for k, v in local.extra_disks : k => v if v.node == each.key }
    content {
      volume_id = libvirt_volume.extra[disk.key].id
      scsi      = true
      wwn       = disk.value.wwn
    }
  }

  network_interface {
    network_id     = libvirt_network.cluster.id
    mac            = local.node_mac[each.key]
    wait_for_lease = true
  }

  console {
    type        = "pty"
    target_port = "0"
  }

  # VNC, not the provider default (spice) - WSL2's QEMU build has no SPICE support.
  graphics {
    type        = "vnc"
    listen_type = "address"
  }

  lifecycle {
    # A node's disk is a copy-on-write clone of its base image. Changing the base image (a rolling
    # OS upgrade) makes libvirt_volume.node ForceNew - but the provider would otherwise only *update*
    # this domain's disk definition in place, leaving the RUNNING VM on its old disk (old OS, stale
    # kubelet.conf) and unable to free the old volume. Tying the domain's replacement to its volume
    # forces a genuine destroy+recreate, so the VM actually boots the new image. The pinned MAC means
    # it reclaims the same IP. Normal applies don't replace the volume, so this only fires on an
    # image change.
    replace_triggered_by = [libvirt_volume.node[each.key]]

    # Adding or removing an extra disk must NOT rebuild the node.
    #
    # The provider marks `disk` ForceNew at the LIST level, so changing the number of disk blocks
    # ("disk.#") replaces the whole domain - meaning a user attaching storage to a running worker
    # would instead destroy it, wipe its root disk and reschedule its pods. (A change to a nested
    # field like volume_id is NOT ForceNew, which is what the replace_triggered_by above exists to
    # work around - the two facts sit either side of the same schema quirk.)
    #
    # Ignoring `disk` drops that diff entirely, so the disk set of a live domain is ours to manage:
    # the platform hot-attaches/detaches with `virsh attach-disk --live --persistent`, which updates
    # both the running QEMU process and the persistent XML (see internal/provision/tofu.attachDisks).
    # Tofu still owns the VOLUMES above - they are created, destroyed and GC'd with the cluster as
    # normal - only the attachment is converged out of band.
    #
    # This does NOT weaken the OS roll: replace_triggered_by is evaluated independently of the
    # ignored attribute, so a base-image change still replaces the domain, and a REPLACED domain is
    # created from config (ignore_changes only suppresses updates, never creation) - so it comes back
    # with its full extra-disk set already attached. Verified against libvirt 10 / provider 0.8.3.
    ignore_changes = [disk]
  }
}
