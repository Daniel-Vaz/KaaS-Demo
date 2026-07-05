# Each node's extra disks as vCenter reports them back, keyed by the LABEL we gave them
# ("disk-<name>"; the root disk is "disk0" and is never looked up here).
#
# By label rather than by list index, deliberately: the provider matches disk blocks by label/key and
# does NOT guarantee that the disk list in state stays in the order the config declares them. Index
# arithmetic ("the i-th extra disk is disk[i+1]") therefore reads the wrong disk's UUID as soon as a
# node's disks have been added and removed a few times - and that UUID is what Ansible formats a
# device by, so getting it wrong destroys the wrong disk's data. The label is ours and is stable.
#
# A value here is null until vCenter has actually reported the VMDK's UUID, which it has not on the
# tick that creates the disk.
locals {
  node_disk_uuid = {
    for k, vm in vsphere_virtual_machine.node : k => {
      for d in vm.disk : d.label => d.uuid
    }
  }
}

# The same `nodes` contract the libvirt module emits, so one parser serves both backends
# (internal/provision/tofurunner.OutputNodes): a map keyed by node name → {name, ip, mac, disks}.
#
# In dhcp mode the IP is what the guest actually got, as reported by open-vm-tools. In static mode
# it is the address the platform allocated and cloud-init applied - reported from the input rather
# than the guest, so a cluster converges without waiting on the tools' first report.
output "nodes" {
  value = { for k, vm in vsphere_virtual_machine.node : k => {
    name = k
    ip   = var.ip_mode == "static" ? local.nodes[k].ip : try(vm.default_ip_address, "")
    mac  = try(vm.network_interface[0].mac_address, local.node_mac[k])
    # Each extra disk's guest identity, as a hex TOKEN that appears in the /dev/disk/by-id/ symlink
    # udev creates for it (see the same field in the libvirt module's output, and the node_disks role
    # which resolves the device by matching it).
    #
    # vCenter mints the VMDK's UUID, so unlike kvm it is only knowable after the disk exists; it is
    # surfaced in-guest by enable_disk_uuid, and normalising it (dashes stripped, lowercased) is what
    # makes it a substring of the by-id entry.
    #
    # Empty until vCenter reports it - which it does NOT on the tick that creates the disk. That is a
    # normal state, not an error: the Go side drops empty ids, the reconciler won't format a disk it
    # has no identity for, and a later tick observes it. Note try() alone is not enough here: it
    # catches errors, not nulls, so a null uuid would sail through it and blow up replace().
    disks = {
      for d in local.nodes[k].extra_disks :
      d.name => (
        lookup(local.node_disk_uuid[k], "disk-${d.name}", null) == null
        ? ""
        : lower(replace(local.node_disk_uuid[k]["disk-${d.name}"], "-", ""))
      )
    }
  } }
}

output "folder" {
  description = "The per-cluster VM folder holding these nodes"
  value       = vsphere_folder.cluster.path
}
