# One base volume per cluster (from the golden/base image), cloned copy-on-write into a
# per-node volume. cloud-init injects the SSH key + hostname so Ansible can reach each node.
# See docs/networking.md for how the worker reaches these VMs on the libvirt default net.
#
# Written against the dmacvicar/libvirt 0.9 schema, where HCL maps ~1:1 onto libvirt's own XML
# (see docs/resources/*.md upstream, and infra/libvirt/versions.tf for why the 0.8 schema is gone).
# The practical consequence for this module: everything libvirt used to infer from a convenience
# attribute is now spelled out - the network's gateway address and DHCP range, each disk's target
# device and bus, the cloud-init ISO as an ordinary pool volume attached as a CD-ROM.

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
  #   true (remote KVM host): image/base_image are PATHS IN THE POOL on the hypervisor, of volumes
  #     staged there once by the provisioner (internal/kvmhost.StageImage, which reports back the
  #     path it staged to). Nodes back onto them directly; nothing is uploaded through OpenTofu at
  #     all. This is not just an optimisation - staging is once per image rather than once per
  #     cluster, and it moves a multi-GB transfer over a slow link under our own reconcile budget.
  #     See docs/deploy/providers/libvirt.md.
  node_image = { for n in var.nodes : n.name => coalesce(n.image, var.base_image) }
  # Only the import path needs the set of distinct images; preloaded ones already exist in the pool.
  images = var.preloaded_images ? toset([]) : toset(values(local.node_image))
  # The host path each node's root volume backs onto. Imported images are resolved through the
  # volume the provider created for them; preloaded ones are already pool paths.
  node_backing_path = {
    for k, v in local.node_image : k => var.preloaded_images ? v : libvirt_volume.base[v].path
  }
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

  # The same extra disks, grouped per node and in the order the caller sent them, each with the
  # target device it gets when the domain is CREATED. sda is the cloud-init CD-ROM, so these start
  # at sdb; domain.MaxDisksPerNode (8) keeps this far inside the alphabet. The hot-attach path picks
  # its own free target the same way (internal/provision/tofu.freeTarget), so the two agree without
  # sharing state - both just take the lowest sdX the domain is not already using.
  node_extra_disks = {
    for n in var.nodes : n.name => [
      for i, d in n.extra_disks : {
        key    = "${n.name}/${d.name}"
        wwn    = d.wwn
        target = "sd${substr("bcdefghijklmnopqrstuvwxyz", i, 1)}"
      }
    ]
  }

  # The cluster subnet, spelled out. 0.8 took the CIDR and let libvirt place the gateway and the DHCP
  # range; 0.9 maps straight onto <ip>/<dhcp>, so the module states them. These values reproduce what
  # the old provider produced, and the layout is load-bearing: dnsmasq hands out leases from the
  # BOTTOM of the range, while internal/netpool places the HA API VIP and the MetalLB address near
  # the TOP (broadcast-6 and broadcast-7) - so a full-subnet range is what keeps those two reachable
  # on the same L2 without ever being leased in practice.
  net_prefix  = tonumber(split("/", var.network_cidr)[1])
  net_gateway = cidrhost(var.network_cidr, 1)
  net_dhcp_lo = cidrhost(var.network_cidr, 2)
  net_dhcp_hi = cidrhost(var.network_cidr, -2)
}

# One dedicated, isolated network per cluster. A NAT bridge (its own virbrN) gives each cluster
# L2 isolation from every other cluster while still letting the host-networked worker reach the
# nodes (Ansible/kubectl) and the nodes reach the internet (Helm/image pulls). Torn down with the
# cluster on `tofu destroy`.
resource "libvirt_network" "cluster" {
  name      = "${var.cluster_name}-net"
  autostart = true
  forward   = { mode = var.network_mode }
  dns       = { enable = "yes" }

  ips = [{
    family  = "ipv4"
    address = local.net_gateway
    prefix  = local.net_prefix
    dhcp = {
      ranges = [{
        start = local.net_dhcp_lo
        end   = local.net_dhcp_hi
      }]
    }
  }]
}

# Import path only (preloaded_images = false): one COW base volume per distinct image, per cluster.
# Empty when the images are already staged in the pool. `create.content.url` accepts a local path,
# which the provider streams into the pool.
resource "libvirt_volume" "base" {
  for_each = local.images
  name     = "${var.cluster_name}-base-${md5(each.value)}.qcow2"
  pool     = var.pool
  target   = { format = { type = "qcow2" } }
  create   = { content = { url = each.value } }
}

# The creation-time shape of each node's ROOT volume: which golden image it backs onto, and how big
# it is. Both are fixed the moment the volume exists - libvirt has no "change a volume's backing
# file" operation, and the provider says so, refusing every update with "Storage volumes cannot be
# updated. All changes require replacement."
#
# It does not, however, mark backing_store or capacity as forcing replacement, so OpenTofu plans an
# in-place update and the apply then fails. That would break the rolling OS upgrade outright: a node
# re-pointed at a new golden image would fail its reconcile step forever instead of being rebuilt.
# This resource is the missing ForceNew - a change to either input replaces it, which replaces the
# volume, which (via the domain's own replace_triggered_by) rebuilds the VM.
#
# Deliberately NOT done for libvirt_volume.extra below: rebuilding a node's root disk is routine -
# it is a copy-on-write clone of a golden image and the platform replaces nodes on purpose - whereas
# an extra disk holds the user's data and its Longhorn replicas. If an extra disk's immutable shape
# ever did change, failing loudly is the right outcome; silently re-creating it is not.
resource "terraform_data" "node_volume_shape" {
  for_each = local.nodes
  input = {
    backing_path = local.node_backing_path[each.key]
    capacity     = each.value.disk_gb
  }
}

# The node's root disk: an empty qcow2 backed by the golden image, so booting it is a copy-on-write
# clone rather than a copy.
resource "libvirt_volume" "node" {
  for_each = local.nodes
  name     = "${var.cluster_name}-${each.value.name}.qcow2"
  pool     = var.pool
  capacity = each.value.disk_gb * 1024 * 1024 * 1024
  target   = { format = { type = "qcow2" } }
  backing_store = {
    path   = local.node_backing_path[each.key]
    format = { type = "qcow2" }
  }

  lifecycle {
    replace_triggered_by = [terraform_data.node_volume_shape[each.key]]
  }
}

# One volume per extra disk. Blank (no backing store), so the guest sees a raw, unpartitioned device
# that the node_disks Ansible role turns into a PV/VG/LV and mounts.
#
# These are destroyed when the disk leaves var.nodes[].extra_disks - which is exactly why the
# reconciler unmounts and tears the volume group down FIRST (domain.DiskPhaseRemoving): by the time
# this volume disappears, nothing in the guest is using it.
resource "libvirt_volume" "extra" {
  for_each = local.extra_disks
  name     = "${var.cluster_name}-${each.value.node}-${each.value.name}.qcow2"
  pool     = var.pool
  capacity = each.value.size_gb * 1024 * 1024 * 1024
  target   = { format = { type = "qcow2" } }
}

# The cloud-init NoCloud seed. In 0.9 this resource only RENDERS the ISO (locally, at .path); making
# it visible to the hypervisor is an ordinary pool volume that uploads it, attached to the domain as
# a CD-ROM below. The ISO is a few hundred KB, so uploading it through the provider is fine even
# against a remote hypervisor - unlike a multi-GB golden image (see preloaded_images).
resource "libvirt_cloudinit_disk" "node" {
  for_each  = local.nodes
  name      = "${var.cluster_name}-${each.value.name}-cloudinit"
  meta_data = <<-EOT
    instance-id: ${var.cluster_name}-${each.value.name}
    local-hostname: ${each.value.name}
  EOT
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

resource "libvirt_volume" "cloudinit" {
  for_each = local.nodes
  name     = "${var.cluster_name}-${each.value.name}-cloudinit.iso"
  pool     = var.pool
  target   = { format = { type = "raw" } }
  create   = { content = { url = libvirt_cloudinit_disk.node[each.key].path } }
}

resource "libvirt_domain" "node" {
  for_each = local.nodes
  name     = "${var.cluster_name}-${each.value.name}"
  type     = "kvm"

  memory      = each.value.mem_mb
  memory_unit = "MiB"
  vcpu        = each.value.cpus
  running     = true

  os = { type = "hvm" }

  # Expose the real host CPU to the guest. Without this, libvirt/QEMU emulates a baseline `qemu64`
  # model that lacks the x86-64-v2 feature set (SSE4.2, POPCNT, ...), and modern container glibc
  # builds abort on start with "Fatal glibc error: CPU does not support x86-64-v2" - e.g. the kepler
  # add-on crash-loops. host-passthrough is safe here: single host, no live migration.
  cpu = { mode = "host-passthrough" }

  devices = {
    # The root disk FIRST - it is the boot device - then the cloud-init seed, then any extra disks.
    #
    # Extra disks sit on the SCSI bus so each can carry a wwn: libvirt refuses a wwn on virtio ("Only
    # ide and scsi disk support wwn"), and without one the guest has no stable handle on the device -
    # /dev/vdb..vdc are assigned in attach order and renumber when a disk is removed, so a role that
    # formatted "the second disk" would eventually format the wrong one. With scsi+wwn the guest gets
    # /dev/disk/by-id/wwn-0x… , which is what the node_disks role keys on.
    #
    # The extra-disk entries only ever take effect when the domain is CREATED (see the lifecycle
    # block below): a fresh node, or one rebuilt by a rolling OS replacement, boots with its full
    # disk set already attached. Disks added to a LIVE node are hot-attached out of band by the
    # platform, at target devices chosen to avoid the ones spelled here.
    disks = concat(
      [
        {
          device = "disk"
          driver = { name = "qemu", type = "qcow2" }
          source = { file = { file = libvirt_volume.node[each.key].path } }
          target = { dev = "vda", bus = "virtio" }
        },
        {
          device    = "cdrom"
          read_only = true
          driver    = { name = "qemu", type = "raw" }
          source    = { file = { file = libvirt_volume.cloudinit[each.key].path } }
          target    = { dev = "sda", bus = "sata" }
        },
      ],
      [
        for d in local.node_extra_disks[each.key] : {
          device = "disk"
          wwn    = d.wwn
          driver = { name = "qemu", type = "qcow2" }
          source = { file = { file = libvirt_volume.extra[d.key].path } }
          target = { dev = d.target, bus = "scsi" }
        }
      ],
    )

    # A virtio-scsi controller, ALWAYS - even on a node with no extra disks today.
    #
    # libvirt would otherwise add one implicitly, but only when the domain already declares a scsi
    # disk. A node born with none therefore has no scsi controller, and hot-attaching its FIRST extra
    # disk means hot-adding the controller too - which on this machine type (i440fx) libvirt refuses:
    # "Bus 'pci.0' does not support hotplugging". The result is that attaching storage to a running
    # node fails on exactly the nodes that have no storage yet. Declaring the controller up front
    # costs nothing and makes the first attach the same operation as every later one. Like the disks
    # below it, this only lands when the domain is created - a node predating it picks it up the next
    # time it is rebuilt.
    controllers = [{
      type  = "scsi"
      index = 0
      model = "virtio-scsi"
    }]

    interfaces = [{
      mac    = { address = local.node_mac[each.key] }
      model  = { type = "virtio" }
      source = { network = { network = libvirt_network.cluster.name } }
      # Block create until the node has a DHCP lease: the module's `nodes` output reports that IP
      # back to the provisioner, and everything downstream (Ansible inventory, the API VIP's subnet,
      # etcd/cert SANs) is keyed on it.
      wait_for_ip = { source = "lease", timeout = 300 }
    }]

    # A serial console, so `virsh console` reaches a node whose network never came up - the first
    # thing worth reading when cloud-init fails.
    consoles = [{
      target = { type = "serial", port = 0 }
    }]

    # VNC, not the provider default (spice) - WSL2's QEMU build has no SPICE support.
    graphics = [{
      vnc = { auto_port = true, listen = "127.0.0.1" }
    }]
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

    # Adding or removing an extra disk must NOT rewrite this domain's device list.
    #
    # OpenTofu would otherwise converge the disk list here, and the provider converges it by
    # redefining the domain - which updates the PERSISTENT XML only. The running QEMU process would
    # not gain (or lose) the device until the node rebooted, so "attach storage to this worker" would
    # silently do nothing until something else restarted the VM. So the module declares the disks (a
    # domain created fresh, or rebuilt by an OS roll, comes up with its full set) and ignores changes
    # to them; the live delta is applied with `virsh attach-disk --live --persistent`, which updates
    # both copies at once. See internal/provision/tofu.attachDisks.
    #
    # Tofu still owns the VOLUMES above - they are created, destroyed and GC'd with the cluster as
    # normal - only the attachment is converged out of band.
    #
    # This does NOT weaken the OS roll: replace_triggered_by is evaluated independently of the
    # ignored attribute, so a base-image change still replaces the domain, and a REPLACED domain is
    # created from config (ignore_changes only suppresses updates, never creation) - so it comes back
    # with its full extra-disk set already attached.
    ignore_changes = [devices]
  }
}

# Each node's DHCP lease, read back for the `nodes` output. 0.8 exposed the addresses on the domain
# itself; 0.9 splits the query off into its own data source (lifecycle vs. query - a resource creates
# and destroys, a data source observes), which is also why it can be read on a later refresh without
# touching the domain.
data "libvirt_domain_interface_addresses" "node" {
  for_each = local.nodes
  domain   = libvirt_domain.node[each.key].name
  source   = "lease"
}
