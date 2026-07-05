# The cluster's dedicated network - name/bridge/CIDR/mode, for `tofu output` and debugging.
output "network" {
  value = {
    name   = libvirt_network.cluster.name
    bridge = libvirt_network.cluster.bridge
    cidr   = var.network_cidr
    mode   = var.network_mode
  }
}

# Per-node observed state, read back by the Go wrapper via `tofu output -json` and mapped
# to provision.ProvisionedNode. IP comes from the DHCP lease on the libvirt network.
output "nodes" {
  value = {
    # key by the short node name (matches provision.NodeSpec.VMName), not the composed
    # libvirt domain name - the Go wrapper matches nodes back to specs by this.
    for k, d in libvirt_domain.node : k => {
      name = k
      ip   = try(d.network_interface[0].addresses[0], "")
      mac  = try(d.network_interface[0].mac, "")
      # Each extra disk's guest identity, as a hex TOKEN that appears in the /dev/disk/by-id/ symlink
      # udev creates for it - here "5000c50…", which lands in /dev/disk/by-id/wwn-0x5000c50… . The
      # node_disks role resolves the device by matching this token rather than by an exact path, so
      # the same mechanism serves vSphere (where vCenter mints a UUID instead; see that module).
      # Known up front here, since the platform chose the wwn - but reported through the same output
      # so everything above the provision seam stays provider-agnostic.
      disks = {
        for dk, dv in local.extra_disks : dv.name => replace(dv.wwn, "0x", "") if dv.node == k
      }
    }
  }
}

# Where each extra disk's volume actually lives, and the identity to attach it under. Consumed by the
# Go wrapper's hot-attach step (internal/provision/tofu.attachDisks): the domain's `disk` blocks are
# ignore_changes'd, so adding a disk to a LIVE node is done with virsh, which needs the volume path.
# Keyed "<node>/<disk>", matching local.extra_disks.
output "extra_disks" {
  value = {
    for k, v in local.extra_disks : k => {
      node   = v.node
      name   = v.name
      wwn    = v.wwn
      volume = libvirt_volume.extra[k].id # the pool path, e.g. /var/lib/libvirt/images/…qcow2
    }
  }
}
