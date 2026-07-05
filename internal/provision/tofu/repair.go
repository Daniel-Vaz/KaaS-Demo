package tofu

import (
	"context"
	"fmt"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
)

// This file implements provision.NodePowerer and provision.NodeReplacer for libvirt/KVM - the two
// capabilities automatic node repair leans on (see internal/domain.RepairPolicy).

// NodePower reports which of the cluster's domains libvirt currently has running, keyed by VM name.
//
// `virsh list --all` rather than a tofu refresh: this is polled for every Ready cluster on a repair
// cadence, and a refresh would mean running OpenTofu - and taking its state lock - against every
// workspace on the platform every few minutes. It is also the more honest source: tofu's state
// records what was created, libvirt records what is running, and the difference between those two is
// precisely the fault being looked for.
func (p *Provisioner) NodePower(ctx context.Context, clusterID string) (map[string]bool, error) {
	// silent: a power poll runs constantly and its output is a domain table - log material, not
	// cluster-timeline material.
	out, err := procstream.Capture(ctx, "", nil, silent, "virsh", "-c", p.cfg.LibvirtURI, "list", "--all")
	if err != nil {
		return nil, fmt.Errorf("tofu: virsh list: %w", err)
	}
	prefix := clusterID + "-"
	power := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		// " Id   Name          State" / " -    test-cp-0     shut off"
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] == "Id" {
			continue
		}
		name := fields[1]
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		state := strings.Join(fields[2:], " ")
		// "running" is the only state that serves. "paused" and "pmsuspended" are deliberately NOT
		// running: a paused domain answers no packets, which is indistinguishable from a dead one
		// from inside the cluster, and starting it is the same cheap repair.
		power[strings.TrimPrefix(name, prefix)] = state == "running"
	}
	return power, nil
}

// PowerOnNode starts one domain. Idempotent: virsh reports an already-active domain as an error,
// which is translated back into success rather than surfaced - "make this VM run" is satisfied.
func (p *Provisioner) PowerOnNode(ctx context.Context, clusterID, vmName string) error {
	dom := clusterID + "-" + vmName
	err := procstream.Run(ctx, "", nil, p.run.EmitFor(clusterID), "virsh", "-c", p.cfg.LibvirtURI, "start", dom)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already active") {
		return nil
	}
	return err
}

// ReplaceNode rebuilds one node's VM from scratch: OpenTofu destroys and re-creates both the domain
// and its ROOT VOLUME, then the caller rejoins the node.
//
// Replacing the volume as well as the domain is the load-bearing half. The module's root volume is a
// copy-on-write clone of the golden image, and the domain merely points at it - so replacing the
// domain alone hands the rebuilt VM back the same corrupt or full filesystem it was replaced FOR.
// Replacing the volume is what makes this a rebuild rather than a reboot.
//
// The node's EXTRA disks are deliberately left out of the address list. Their volumes are separate
// resources holding the node's actual data (and its Longhorn replicas); they already survive a
// rolling OS replacement, and the node_disks role re-adopts them - mkfs is guarded by a blkid check
// - so a repaired node comes back with its storage intact. Including them here would silently make
// automatic repair destructive to user data, which is the one thing it must never be.
func (p *Provisioner) ReplaceNode(ctx context.Context, clusterID, vmName string) error {
	ws, err := p.run.EnsureWorkspace(clusterID)
	if err != nil {
		return fmt.Errorf("tofu: %w", err)
	}
	// The module keys these resources with for_each on the node name (infra/libvirt/main.tf), so the
	// addresses are stable across scale operations - unlike a count index, which shifts under every
	// node added or removed and would point -replace at the wrong VM.
	addrs := []string{
		fmt.Sprintf("libvirt_domain.node[%q]", vmName),
		fmt.Sprintf("libvirt_volume.node[%q]", vmName),
		// The cloud-init disk is per-node and cheap; re-creating it alongside guarantees the rebuilt
		// domain gets a freshly rendered config rather than a stale one from a previous shape.
		fmt.Sprintf("libvirt_cloudinit_disk.node[%q]", vmName),
	}
	// Same ordering contract as EnsureNodes, and it bites harder here: apply is about to destroy this
	// domain, and a domain destroyed while an extra disk is attached leaves tofu holding a volume
	// reference it can no longer resolve - which wedges every later refresh, including destroy. So
	// detach first, replace, then re-attach.
	if err := p.detachDisksForNode(ctx, clusterID, vmName); err != nil {
		return fmt.Errorf("tofu: detach disks before replace: %w", err)
	}
	if _, err := p.run.ApplyReplacing(ctx, ws, clusterID, addrs); err != nil {
		return err
	}
	if err := p.attachNewDisks(ctx, clusterID, ws, nil); err != nil {
		return fmt.Errorf("tofu: re-attach disks after replace: %w", err)
	}
	return nil
}

// detachDisksForNode detaches every extra disk currently attached to one domain, leaving its volumes
// alone. Best-effort per disk in the same way detachExtraDisksBeforeDestroy is: a domain that is
// already gone, or a disk already detached, is the state we are trying to reach.
func (p *Provisioner) detachDisksForNode(ctx context.Context, clusterID, vmName string) error {
	dom := clusterID + "-" + vmName
	attached, running, err := p.domainDisks(ctx, dom)
	if err != nil {
		// No such domain - the VM is already gone, which is a legitimate way to arrive at a repair
		// (something destroyed it out of band). Nothing to detach; let apply re-create it.
		return nil
	}
	for wwn, target := range attached {
		if err := p.virsh(ctx, clusterID, running, "detach-disk", dom, target); err != nil {
			return fmt.Errorf("detach %s (%s): %w", target, wwn, err)
		}
	}
	return nil
}
