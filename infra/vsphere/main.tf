# Each cluster is a folder of VMs under the deployment's parent folder, every VM a clone of the
# per-(OS, Kubernetes) golden TEMPLATE that Packer baked (see packer/vsphere-ubuntu-k8s.pkr.hcl).
# Unlike the libvirt module there is no per-cluster network: every cluster shares the operator's
# portgroup, so what varies is only how a node LEARNS its address - see var.ip_mode.

locals {
  nodes = { for n in var.nodes : n.name => n }

  # One data source per distinct template (nodes of a cluster mid-OS-upgrade straddle two).
  templates = toset([for n in var.nodes : n.image])

  # Every extra disk across every node, flattened to one map keyed "<node>/<disk>" so each becomes its
  # OWN vsphere_virtual_disk resource (below) rather than an inline block of the VM.
  #
  # This is what makes a node's extra disks survive that node's VM being rebuilt. A repair (and a
  # rolling OS upgrade) replaces the VM with `tofu apply -replace`; an inline disk is part of the VM
  # resource and would be destroyed with it, but a standalone vsphere_virtual_disk is its own resource,
  # untouched by the VM's replacement, and the recreated VM re-attaches it by path. It is the exact
  # shape the libvirt module already uses (a libvirt_volume per extra disk, attached out of band) - so
  # all three providers now mean the same thing by "rebuild the node, keep its data".
  #
  # unit_number is fixed by the disk's index in the node's (name-ordered) list - 0 is the root disk,
  # so extras start at 1 - and never reordered, so a disk keeps its SCSI address across edits.
  extra_disks = merge([
    for n in var.nodes : {
      for i, d in n.extra_disks : "${n.name}/${d.name}" => {
        node        = n.name
        name        = d.name
        size_gb     = d.size_gb
        unit_number = i + 1
      }
    }
  ]...)

  # A deterministic MAC per node, in VMware's 00:50:56:00–3f range (vCenter REJECTS a static MAC
  # outside it) - hence the mask to 6 bits on the first derived byte. Pinning the MAC is what makes
  # a VM re-created for a rolling OS upgrade reclaim the SAME DHCP lease, so the certs and etcd
  # membership keyed on the node's IP stay valid. It is load-bearing in dhcp mode, and harmless
  # (but kept, for a stable identity) in static mode, where it also anchors the cloud-init netplan
  # match below.
  node_mac = { for n in var.nodes : n.name => format("00:50:56:%02x:%s:%s",
    parseint(substr(md5("${var.cluster_id}-${n.name}"), 0, 2), 16) % 64,
    substr(md5("${var.cluster_id}-${n.name}"), 2, 2),
    substr(md5("${var.cluster_id}-${n.name}"), 4, 2))
  }

  prefix_len = tonumber(split("/", var.network_cidr)[1])

  # cloud-init metadata. The VMware datasource (DataSourceVMware, shipped in Ubuntu's cloud images
  # alongside open-vm-tools) reads these guestinfo keys at first boot. In static mode the network
  # config is a netplan v2 block matched on the pinned MAC - matching by MAC rather than interface
  # name because the guest's NIC name is not ours to predict.
  metadata = { for k, n in local.nodes : k => yamlencode(merge(
    {
      instance-id    = "${var.cluster_id}-${n.name}"
      local-hostname = n.name
    },
    var.ip_mode == "static" ? {
      network = {
        version = 2
        ethernets = {
          id0 = {
            match       = { macaddress = local.node_mac[k] }
            dhcp4       = false
            addresses   = ["${n.ip}/${local.prefix_len}"]
            gateway4    = var.gateway
            nameservers = { addresses = var.dns }
          }
        }
      }
    } : {}
  )) }

  # Same cloud-config the libvirt module injects: the kaas user + SSH key Ansible connects as.
  userdata = { for k, n in local.nodes : k => <<-EOT
    #cloud-config
    hostname: ${n.name}
    fqdn: ${n.name}
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
}

data "vsphere_datacenter" "dc" {
  name = var.datacenter
}

data "vsphere_compute_cluster" "cluster" {
  name          = var.compute_cluster
  datacenter_id = data.vsphere_datacenter.dc.id
}

data "vsphere_datastore" "ds" {
  name          = var.datastore
  datacenter_id = data.vsphere_datacenter.dc.id
}

data "vsphere_network" "net" {
  name          = var.network
  datacenter_id = data.vsphere_datacenter.dc.id
}

# The golden images: VM templates in the parent folder, one per (OS, Kubernetes) pair. A missing
# one fails the plan - and the reconciler preflights it before ever draining a node for a rolling
# OS replacement (internal/provision/vsphere.ImageAvailable).
data "vsphere_virtual_machine" "template" {
  for_each      = local.templates
  name          = "${var.parent_folder}/${each.value}"
  datacenter_id = data.vsphere_datacenter.dc.id
}

# Everything a cluster owns lives in its own folder, named for the cluster and its id, under the
# deployment's parent folder. Destroyed with the cluster.
resource "vsphere_folder" "cluster" {
  path          = "${var.parent_folder}/${var.folder_name}"
  type          = "vm"
  datacenter_id = data.vsphere_datacenter.dc.id
}

# One standalone VMDK per extra disk, in a per-cluster directory on the datastore. Independent of any
# VM's lifecycle: a node's VM can be destroyed and rebuilt (a repair, a rolling OS upgrade) and these
# files are untouched - the recreated VM re-attaches them (see the disk{ attach = true } block below).
# Destroyed with the cluster, and only then, because the VM that attaches one holds a dependency on it.
resource "vsphere_virtual_disk" "extra" {
  for_each = local.extra_disks

  size               = each.value.size_gb
  type               = "thin"
  datacenter         = var.datacenter
  datastore          = var.datastore
  vmdk_path          = "${var.folder_name}/${each.value.node}-${each.value.name}.vmdk"
  create_directories = true
}

resource "vsphere_virtual_machine" "node" {
  for_each = local.nodes

  # Prefixed with the cluster id: VM names are unique platform-wide even though two tenants may
  # each name a cluster "dev".
  name             = "${var.cluster_id}-${each.value.name}"
  annotation       = "KaaS cluster ${var.cluster_name} (${var.cluster_id}) - ${each.value.role}"
  folder           = vsphere_folder.cluster.path
  resource_pool_id = data.vsphere_compute_cluster.cluster.resource_pool_id
  datastore_id     = data.vsphere_datastore.ds.id

  num_cpus = each.value.cpus
  memory   = each.value.mem_mb

  guest_id  = data.vsphere_virtual_machine.template[each.value.image].guest_id
  scsi_type = data.vsphere_virtual_machine.template[each.value.image].scsi_type
  firmware  = data.vsphere_virtual_machine.template[each.value.image].firmware

  network_interface {
    network_id     = data.vsphere_network.net.id
    adapter_type   = data.vsphere_virtual_machine.template[each.value.image].network_interface_types[0]
    use_static_mac = true
    mac_address    = local.node_mac[each.key]
  }

  # Surface each virtual disk's UUID to the guest, so udev publishes it under /dev/disk/by-id/. That
  # is the only way the node_disks Ansible role can tell one extra disk from another - without it the
  # guest sees anonymous /dev/sdb, /dev/sdc… assigned in bus order, which shifts when a disk is
  # removed. The kvm module gets the same property from a pinned wwn; here vCenter owns the identity,
  # so we enable it and read it back (see the extra_disks output).
  enable_disk_uuid = true

  disk {
    label = "disk0"
    # Never shrink below the template's disk - vSphere cannot, and the clone would fail.
    size             = max(each.value.disk_gb, data.vsphere_virtual_machine.template[each.value.image].disks[0].size)
    thin_provisioned = data.vsphere_virtual_machine.template[each.value.image].disks[0].thin_provisioned
    eagerly_scrub    = data.vsphere_virtual_machine.template[each.value.image].disks[0].eagerly_scrub
  }

  # Extra disks, ATTACHED rather than created inline. Each is a standalone vsphere_virtual_disk
  # (above); attaching it - rather than declaring `size` here - is what keeps a node's data off the VM
  # resource, so `tofu apply -replace` on the VM rebuilds the node without touching its disks. `attach`
  # implies keep-on-remove, so detaching one (this VM being destroyed) never deletes the VMDK; the file
  # is destroyed only with its own resource, when the disk leaves the desired set or the cluster does.
  #
  # The vSphere provider still adds and removes disks on a running VM in place, so a disk attached to or
  # detached from a live node needs no lifecycle workaround (unlike libvirt's virsh dance). unit_number
  # is the SCSI address, stable per disk from its index in the node's (name-ordered) list - 0 is the
  # root disk, so extras start at 1. The label is unchanged ("disk-<name>"), so the UUID read-back in
  # outputs.tf, which keys on it, is unaffected - and enable_disk_uuid surfaces an attached disk's UUID
  # exactly as it does a created one.
  dynamic "disk" {
    for_each = { for k, d in local.extra_disks : k => d if d.node == each.key }
    content {
      label        = "disk-${disk.value.name}"
      attach       = true
      path         = vsphere_virtual_disk.extra[disk.key].vmdk_path
      datastore_id = data.vsphere_datastore.ds.id
      unit_number  = disk.value.unit_number
    }
  }

  # clone{} is ForceNew: pointing a node at a different template (a rolling OS upgrade) replaces
  # that one VM outright. No replace_triggered_by lifecycle hack is needed here, unlike the libvirt
  # module - the provider does the right thing natively.
  clone {
    template_uuid = data.vsphere_virtual_machine.template[each.value.image].id
  }

  # cloud-init through guestinfo rather than a customization spec: it is the same cloud-init
  # contract the KVM path uses (one user-data document), so one golden-image playbook and one set
  # of assumptions serve both providers.
  extra_config = {
    "guestinfo.metadata"          = base64encode(local.metadata[each.key])
    "guestinfo.metadata.encoding" = "base64"
    "guestinfo.userdata"          = base64encode(local.userdata[each.key])
    "guestinfo.userdata.encoding" = "base64"
  }

  # In dhcp mode the node's address is only knowable once the guest has booted and open-vm-tools
  # reports it - and it is the output the whole control plane keys on, so block until it exists. In
  # static mode we already know the address, so wait only for the tools to come up.
  wait_for_guest_net_timeout  = var.ip_mode == "dhcp" ? 10 : 0
  wait_for_guest_net_routable = var.ip_mode == "dhcp"
  wait_for_guest_ip_timeout   = var.ip_mode == "dhcp" ? 0 : 10
}
