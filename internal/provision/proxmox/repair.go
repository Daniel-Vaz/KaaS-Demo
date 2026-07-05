package proxmox

import (
	"context"
	"fmt"
)

// ReplaceNode implements provision.NodeReplacer for Proxmox VE: rebuild ONE node's VM from scratch,
// leaving the rest of the cluster untouched. Used by automatic node repair's last rung and by the
// rolling OS upgrade.
//
// The root disk is the VM's own scsi0 block, so replacing the VM gives the node a fresh root - which
// is the point of a rebuild. The node's EXTRA disks are NOT inline: each lives on the per-cluster
// disk-owner VM and is attached to the node by path_in_datastore (see infra/proxmox/main.tf), so the
// node VM's replacement leaves the owner's volumes - and the data on them - untouched, and the
// recreated node re-attaches them. bpg preserves an attached (another VM's) volume across the
// consuming VM's recreation by design, so this is safe on a disk-bearing node, uniformly with libvirt
// and vSphere.
//
// SHORTCUT: the disk-owner pattern uses bpg's path_in_datastore attach, which is marked experimental
// upstream, and this backend is still awaiting real-hardware validation - so the disk-preserving
// rebuild here is validated by `tofu validate` and the fake, not yet against a live Proxmox cluster.
func (p *Provisioner) ReplaceNode(ctx context.Context, clusterID, vmName string) error {
	ws, err := p.run.EnsureWorkspace(clusterID)
	if err != nil {
		return fmt.Errorf("proxmox: %w", err)
	}
	// for_each is keyed on the node name (infra/proxmox/main.tf), so the address survives scaling.
	addr := fmt.Sprintf("proxmox_virtual_environment_vm.node[%q]", vmName)
	_, err = p.run.ApplyReplacing(ctx, ws, clusterID, []string{addr})
	return err
}

// NOTE: like the vSphere backend, this one deliberately does not implement provision.NodePowerer -
// see internal/provision/vsphere/repair.go for the reasoning. Automatic repair works here without
// it, minus the FaultVMDown class.
