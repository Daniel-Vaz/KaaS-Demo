package vsphere

import (
	"context"
	"fmt"
)

// ReplaceNode implements provision.NodeReplacer for vSphere: rebuild ONE node's VM from scratch,
// leaving the rest of the cluster untouched. Used by automatic node repair's last rung and by the
// rolling OS upgrade.
//
// The root disk is part of the VM resource here, so replacing the VM replaces the root with it -
// there is no separate volume to remember to include, and no way to accidentally hand the rebuilt
// node back its old filesystem. The node's EXTRA disks are the opposite: each is a standalone
// vsphere_virtual_disk (see infra/vsphere/main.tf), attached to the VM rather than declared inside
// it, so the VM's replacement leaves them - and the data on them - untouched, and the recreated VM
// re-attaches them by path. That is what makes this rung safe on a disk-bearing node, uniformly with
// libvirt and Proxmox: "rebuild the node, keep its data" means the same thing on all three.
func (p *Provisioner) ReplaceNode(ctx context.Context, clusterID, vmName string) error {
	ws, err := p.run.EnsureWorkspace(clusterID)
	if err != nil {
		return fmt.Errorf("vsphere: %w", err)
	}
	// for_each is keyed on the node name (infra/vsphere/main.tf), so the address is stable across
	// scale operations - a count index would shift under every node added or removed.
	addr := fmt.Sprintf("vsphere_virtual_machine.node[%q]", vmName)
	_, err = p.run.ApplyReplacing(ctx, ws, clusterID, []string{addr})
	return err
}

// NOTE: this backend deliberately does NOT implement provision.NodePowerer. Reading a VM's power
// state means a vCenter API call, and every other thing this provisioner does goes through
// OpenTofu - adding a govmomi client here for one poll would put a second, differently-authenticated
// path to vCenter in the worker. The capability is optional by design: without it automatic repair
// simply never produces the FaultVMDown class on vSphere and starts at its next rung, which costs
// only the ability to distinguish "the VM is off" from "the node is NotReady". Production would use
// govmomi (or the Proxmox API on that backend) and gain the cheapest repair rung back.
