# The same `nodes` contract the libvirt and vSphere modules emit, so one parser serves every backend
# (internal/provision/tofurunner.OutputNodes): a map keyed by node name → {name, ip, mac, disks}.
#
# In dhcp mode the IP is what the guest actually got, as reported by the QEMU guest agent
# (ipv4_addresses is a list-per-interface; we take the first non-loopback IPv4). In static mode it is
# the address the platform allocated and cloud-init applied - reported from the input rather than the
# guest, so a cluster converges without waiting on the agent's first report.
output "nodes" {
  value = { for k, vm in proxmox_virtual_environment_vm.node : k => {
    name = k
    ip = var.ip_mode == "static" ? local.nodes[k].ip : try(
      [for ips in vm.ipv4_addresses : ips[0] if length(ips) > 0 && !startswith(ips[0], "127.")][0],
    "")
    mac = local.node_mac[k]
    # Each extra disk's guest identity: the platform-minted serial token, which appears in the
    # /dev/disk/by-id/ symlink udev creates for the disk (see the same field in the other modules'
    # output, and the node_disks role which resolves the device by matching it). Known up front (we
    # chose the serial), so unlike vSphere there is no "empty until observed" tick.
    disks = local.disk_serial[k]
  } }
}
