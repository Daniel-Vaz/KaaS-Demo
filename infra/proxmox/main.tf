# Each cluster is a set of VMs on ONE Proxmox node, every VM a full clone of the per-(OS,
# Kubernetes) golden TEMPLATE that Packer baked (see packer/proxmox/ubuntu-k8s.pkr.hcl). Like the
# vSphere module - and unlike libvirt - there is no per-cluster network: every cluster shares the
# operator's bridge, so what varies is only how a node LEARNS its address (var.ip_mode). Node
# addressing, the kaas user and its SSH key are all injected through Proxmox's NATIVE cloud-init
# (the initialization block), so no snippets / SSH-to-host are needed - the API alone suffices.

locals {
  nodes = { for n in var.nodes : n.name => n }

  # One template lookup per distinct image (nodes of a cluster mid-OS-upgrade straddle two).
  templates = toset([for n in var.nodes : n.image])

  prefix_len = tonumber(split("/", var.network_cidr)[1])

  # A deterministic MAC per node, in Proxmox's own OUI (BC:24:11). Pinning it is what makes a VM
  # re-created for a rolling OS upgrade reclaim the SAME DHCP lease, so the certs and etcd membership
  # keyed on the node's IP stay valid - load-bearing in dhcp mode, harmless (kept for a stable
  # identity) in static mode.
  node_mac = { for n in var.nodes : n.name => upper(format("BC:24:11:%s:%s:%s",
    substr(md5("${var.cluster_id}-${n.name}"), 0, 2),
    substr(md5("${var.cluster_id}-${n.name}"), 2, 2),
    substr(md5("${var.cluster_id}-${n.name}"), 4, 2)))
  }

  # Each extra disk's guest identity: the platform-minted wwn, "0x"-stripped, used BOTH as the
  # QEMU disk serial (below) and reported back as the device id. udev publishes a serial-bearing
  # scsi disk at /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_<serial>, and <serial> is exactly this
  # token - which is what the node_disks Ansible role greps for. Deriving it from the wwn we already
  # mint (rather than reading an identity back, as vsphere must) keeps it knowable before the disk
  # exists.
  disk_serial = { for n in var.nodes : n.name => {
    for d in n.extra_disks : d.name => replace(d.wwn, "0x", "")
  } }

  # Every extra disk across every node, flattened and globally indexed. Each becomes a volume on the
  # per-cluster disk-OWNER VM (below), NOT an inline disk of the node it serves - which is what lets a
  # node's VM be rebuilt (a repair, a rolling OS upgrade) without destroying its data. bpg has no
  # standalone disk resource, so the owner VM plays that role: a never-started VM that holds the
  # volumes, exactly the pattern the provider documents for disks that must outlive a consuming VM's
  # recreation. The node then ATTACHES its own by path_in_datastore. This is the Proxmox equivalent of
  # the libvirt libvirt_volume / vsphere vsphere_virtual_disk resources - all three keep extra-disk
  # storage off the node VM so `tofu apply -replace` on that VM is non-destructive to it.
  #
  # global_slot fixes the disk's scsi address on the OWNER (so its volume name and reported path are
  # stable); node_slot fixes it on the NODE (0 is the node's root disk, so extras start at scsi1).
  extra_flat = flatten([
    for n in var.nodes : [
      for i, d in n.extra_disks : {
        node      = n.name
        name      = d.name
        size_gb   = d.size_gb
        serial    = replace(d.wwn, "0x", "")
        node_slot = i + 1
      }
    ]
  ])
  extra_by_gi = { for gi, d in local.extra_flat : gi => d }

  # The owner VM's disks read back as a list; re-key it by the scsi interface WE assigned so a node
  # looks its disk up by a stable name rather than a list position (bpg does not promise the disk list
  # stays in declaration order). Empty until the owner exists, which is fine - a cluster with no extra
  # disks creates no owner and no node references this.
  owner_disk_by_iface = length(local.extra_flat) > 0 ? {
    for d in proxmox_virtual_environment_vm.disk_owner[0].disk : d.interface => d
  } : {}
}

# Resolve each golden-image template NAME to its numeric vm_id on the target node - the clone source.
# A missing template yields an empty vms list and fails the plan at the vm_id lookup below, and the
# reconciler preflights it before ever draining a node for a rolling OS replacement
# (internal/provision/proxmox.ImageAvailable).
data "proxmox_virtual_environment_vms" "template" {
  for_each = local.templates

  node_name = var.node_name
  filter {
    name   = "name"
    values = [each.value]
  }
  filter {
    name   = "template"
    values = ["true"]
  }
}

# The per-cluster disk-owner: a VM that exists only to OWN the extra-disk volumes, so they are not
# inline disks of any node and therefore survive a node's VM being rebuilt (a repair, a rolling OS
# upgrade). It is NEVER started (started + on_boot false) and holds no OS - two VMs must not write the
# same block device, and only the node VMs ever mount these. Created only when the cluster actually has
# extra disks, and destroyed with the cluster (which is the one time these volumes should be deleted).
resource "proxmox_virtual_environment_vm" "disk_owner" {
  count = length(local.extra_flat) > 0 ? 1 : 0

  name        = "${var.cluster_id}-diskowner"
  description = "KaaS cluster ${var.cluster_name} (${var.cluster_id}) - extra-disk volume owner (never started)"
  node_name   = var.node_name
  tags        = ["kaas", var.cluster_id, "diskowner"]

  started         = false
  on_boot         = false
  stop_on_destroy = true

  cpu {
    cores = 1
  }
  memory {
    dedicated = 512
  }

  # One volume per extra disk, at the scsi slot named by its global index - so owner_disk_by_iface can
  # re-key the disk list by that stable interface name (bpg does not promise list order). The node that
  # uses it attaches it by the resulting path_in_datastore.
  dynamic "disk" {
    for_each = local.extra_by_gi
    content {
      datastore_id = var.datastore
      interface    = "scsi${disk.key}"
      size         = disk.value.size_gb
    }
  }
}

resource "proxmox_virtual_environment_vm" "node" {
  for_each = local.nodes

  # Prefixed with the cluster id: VM names are unique cluster-wide even though two tenants may each
  # name a cluster "dev". Proxmox VM names are DNS-label-shaped, which our ids and pool names satisfy.
  name          = "${var.cluster_id}-${each.value.name}"
  description   = "KaaS cluster ${var.cluster_name} (${var.cluster_id}) - ${each.value.role}"
  node_name     = var.node_name
  tags          = ["kaas", var.cluster_id]
  scsi_hardware = "virtio-scsi-single"

  # Graceful ACPI shutdown before destroy, so a `tofu destroy` (delete + orphan GC) never wedges on
  # a still-running guest.
  stop_on_destroy = true

  clone {
    vm_id = data.proxmox_virtual_environment_vms.template[each.value.image].vms[0].vm_id
    full  = true
  }

  agent {
    enabled = true # the guest agent (baked by Packer) reports the node's DHCP address back
  }

  cpu {
    cores = each.value.cpus
    type  = "host"
  }

  memory {
    dedicated = each.value.mem_mb
  }

  # Root disk: the cloned template's scsi0, grown to the requested size (Proxmox can only grow a
  # clone's disk, never shrink it) and placed on the operator's datastore.
  disk {
    datastore_id = var.datastore
    interface    = "scsi0"
    size         = each.value.disk_gb
  }

  # Extra disks, ATTACHED from the disk-owner VM rather than created inline. Attaching the owner's
  # volume by path_in_datastore - instead of declaring `size` here, which would make it this VM's own
  # disk - is what keeps a node's data off the node resource, so `tofu apply -replace` on the node
  # rebuilds it (fresh root disk) while the owner's volumes, and the data on them, are untouched.
  # bpg preserves an attached (another VM's) volume across the consuming VM's recreation by design;
  # only the owner being destroyed (i.e. the cluster being deleted) removes these.
  #
  # Proxmox still hot-adds/removes disks in place, so attaching to or detaching from a live node needs
  # no lifecycle workaround. The scsi slot is the node-local index (0 is the root disk, so extras start
  # at scsi1); the serial is the stable /dev/disk/by-id identity the node_disks role resolves through.
  dynamic "disk" {
    for_each = { for gi, d in local.extra_by_gi : gi => d if d.node == each.key }
    content {
      datastore_id      = var.datastore
      path_in_datastore = local.owner_disk_by_iface["scsi${disk.key}"].path_in_datastore
      file_format       = local.owner_disk_by_iface["scsi${disk.key}"].file_format
      size              = local.owner_disk_by_iface["scsi${disk.key}"].size
      interface         = "scsi${disk.value.node_slot}"
      serial            = disk.value.serial
    }
  }

  # Native cloud-init: node addressing + the kaas user Ansible connects as. Proxmox's cloud-init
  # grants the created user passwordless sudo, matching the kaas account the KVM/vSphere paths inject
  # via a #cloud-config document - so one Ansible layer serves all three providers.
  initialization {
    datastore_id = var.datastore

    dynamic "dns" {
      for_each = length(var.dns) > 0 ? [1] : []
      content {
        servers = var.dns
      }
    }

    ip_config {
      ipv4 {
        address = var.ip_mode == "static" ? "${each.value.ip}/${local.prefix_len}" : "dhcp"
        gateway = var.ip_mode == "static" ? var.gateway : null
      }
    }

    user_account {
      username = "kaas"
      keys     = [trimspace(var.ssh_authorized_key)]
    }
  }

  network_device {
    bridge      = var.bridge
    mac_address = local.node_mac[each.key]
    # On a VLAN-aware bridge the node network usually lives on a tagged VLAN; 0 leaves the NIC
    # untagged (a plain access bridge).
    vlan_id = var.vlan > 0 ? var.vlan : null
  }

  # In dhcp mode the address is only knowable once the guest agent reports it, and it is the output
  # the whole control plane keys on - the ipv4_addresses read below blocks on it. Nothing here needs
  # replacing when a node is rolled onto a new image: clone{ vm_id } is ForceNew, so pointing a node
  # at a different template rebuilds that one VM, natively.
  lifecycle {
    ignore_changes = [
      # The clone source's post-boot cloud-init state is Proxmox's to manage; only OUR inputs (size,
      # image, network) should drive changes.
      started,
    ]
  }
}
