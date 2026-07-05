package tofu

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

// Extra-disk ATTACHMENT on libvirt is converged here rather than by OpenTofu - the one piece of
// infrastructure this backend does not express as a resource.
//
// The reason is a provider quirk: dmacvicar/libvirt marks libvirt_domain's `disk` ForceNew at the
// LIST level, so adding or removing a disk block changes `disk.#` and REPLACES the whole domain.
// Taken literally, "attach a disk to this worker" would instead destroy the worker, wipe its root
// disk and reschedule its pods. So the module declares the disks (a domain created fresh, or rebuilt
// by an OS roll, comes up with its full set) but marks `disk` ignore_changes, and the live delta is
// applied with virsh.
//
// The ordering around `apply` is load-bearing, and getting it wrong wedges the cluster:
//
//	OpenTofu owns the VOLUMES. `apply` DELETES the volume of a removed disk and CREATES the volume of
//	an added one. But a domain that still has the disk attached points at that volume's path - so if
//	apply deletes a volume while the disk is still attached, the domain is left referencing a path
//	that no longer exists, and EVERY later refresh fails ("no storage vol with matching path …"),
//	including `tofu destroy`. The cluster can then never be torn down.
//
// So a disk's volume must exist for exactly as long as the disk is attached:
//
//	1. detachRemovedDisks - BEFORE apply. Detach any disk that is attached but no longer desired,
//	   while its volume still exists. Apply then deletes the now-unreferenced volume cleanly.
//	2. apply - deletes removed volumes, creates added ones.
//	3. attachNewDisks - AFTER apply. Attach any desired disk not yet attached, now that apply has
//	   created its volume.
//
// Both passes are CONVERGED (diffed against `virsh dumpxml`), not fired once, so they are idempotent
// and self-healing like the rest of the loop.
//
// SHORTCUT: production would use a provider that models attachment as its own resource, or drive the
// libvirt API directly, rather than shelling out to virsh.

// tfExtraDisk is one entry of the libvirt module's `extra_disks` output: where the volume lives and
// the identity to attach it under.
type tfExtraDisk struct {
	Node   string `json:"node"`
	Name   string `json:"name"`
	WWN    string `json:"wwn"`
	Volume string `json:"volume"`
}

// libvirtDomain is the slice of `virsh dumpxml` this cares about: which disks are attached, at what
// target, under which wwn.
type libvirtDomain struct {
	Devices struct {
		Disks []struct {
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
			WWN string `xml:"wwn"`
		} `xml:"disk"`
	} `xml:"devices"`
}

// detachRemovedDisks runs BEFORE apply. It compares what the LAST apply left attached (the module's
// `extra_disks` output - apply has not changed it yet, so a just-removed disk is still listed there)
// against what each node still desires, and detaches the difference while the volumes still exist.
//
// Reading the desired-set-as-of-last-apply from the module output rather than dumping every domain is
// deliberate: it means a cluster that has never had a disk (the overwhelming majority) does one cheap
// `tofu output` and stops, and only nodes that actually lost a disk are probed with virsh.
func (p *Provisioner) detachRemovedDisks(ctx context.Context, clusterID, ws string, specs []provision.NodeSpec) error {
	prev, err := p.moduleDisks(ctx, ws, clusterID)
	if err != nil {
		// No readable output yet (a fresh workspace before its first apply) means nothing was ever
		// attached, so there is nothing to detach. Not fatal.
		return nil
	}
	if len(prev) == 0 {
		return nil
	}
	desired := desiredWWNs(specs)
	nodes := make([]string, 0, len(prev))
	for n := range prev {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		var stale []tfExtraDisk
		for _, d := range prev[node] {
			if !desired[node][normalizeWWN(d.WWN)] {
				stale = append(stale, d)
			}
		}
		if len(stale) == 0 {
			continue
		}
		dom := domainNameFor(clusterID, node)
		attached, running, err := p.domainDisks(ctx, dom)
		if err != nil {
			// The domain may already be gone (a node also being scaled away this tick), in which case
			// tofu will destroy its volume alongside it - nothing to detach.
			continue
		}
		for _, d := range stale {
			target, ok := attached[normalizeWWN(d.WWN)]
			if !ok {
				continue // already detached (a retried tick)
			}
			if err := p.virsh(ctx, clusterID, running, "detach-disk", dom, target); err != nil {
				return fmt.Errorf("detach disk %q from %s: %w", d.Name, node, err)
			}
			p.run.Emit(clusterID, "info", fmt.Sprintf("node %s: detached disk %q at %s", node, d.Name, target))
		}
	}
	return nil
}

// attachNewDisks runs AFTER apply. For every node that desires a disk, it attaches whatever the
// domain does not already have - the volumes now exist. Only nodes with desired disks are probed, so
// a disk-less cluster does one `tofu output` and stops.
func (p *Provisioner) attachNewDisks(ctx context.Context, clusterID, ws string, _ []provision.NodeSpec) error {
	cur, err := p.moduleDisks(ctx, ws, clusterID)
	if err != nil {
		return err
	}
	if len(cur) == 0 {
		return nil
	}
	nodes := make([]string, 0, len(cur))
	for n := range cur {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		want := cur[node]
		sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
		dom := domainNameFor(clusterID, node)
		attached, running, err := p.domainDisks(ctx, dom)
		if err != nil {
			// The domain may legitimately not be readable yet (created later in this same apply, or
			// mid-boot). Converge on a later tick rather than failing the whole step.
			p.run.Emit(clusterID, "info", fmt.Sprintf("node %s: deferring disk attach - domain not readable yet", node))
			continue
		}
		for _, d := range want {
			if _, have := attached[normalizeWWN(d.WWN)]; have {
				continue // already attached - the converged, no-op case
			}
			target := freeTarget(attached)
			if err := p.virsh(ctx, clusterID, running, "attach-disk", dom, d.Volume, target,
				"--subdriver", "qcow2", "--targetbus", "scsi", "--wwn", d.WWN); err != nil {
				return fmt.Errorf("attach disk %q to %s: %w", d.Name, node, err)
			}
			attached[normalizeWWN(d.WWN)] = target
			p.run.Emit(clusterID, "info", fmt.Sprintf("node %s: attached disk %q at %s", node, d.Name, target))
		}
	}
	return nil
}

// detachExtraDisksBeforeDestroy clears every extra disk off the cluster's domains before `tofu
// destroy` runs. It is best-effort and defensive: a cluster torn down through the normal path already
// has its disks detached, but one that was wedged by an earlier ordering bug (a domain left pointing
// at a since-deleted volume) has a dangling disk that makes tofu's destroy-time refresh fail forever.
// Detaching by target device works even when the volume file is already gone, so this unwedges it.
//
// Domains are discovered by name (the module composes "<clusterID>-<node>"), so this needs no node
// list - which is what makes it usable from DestroyCluster, which only has the cluster ID.
func (p *Provisioner) detachExtraDisksBeforeDestroy(ctx context.Context, clusterID string) {
	doms, err := p.listClusterDomains(ctx, clusterID)
	if err != nil {
		return // virsh unavailable, or nothing provisioned - let tofu destroy proceed
	}
	for _, dom := range doms {
		attached, running, err := p.domainDisks(ctx, dom)
		if err != nil {
			continue
		}
		for _, target := range attached {
			// Best-effort each - the point is to leave the domain XML (both the live and the
			// persistent copy tofu might refresh from) free of a disk whose volume is being deleted.
			if err := p.virsh(ctx, clusterID, running, "detach-disk", dom, target); err != nil {
				p.run.Emit(clusterID, "warn", fmt.Sprintf("pre-destroy: could not detach %s from %s: %v", target, dom, err))
			}
		}
	}
}

// desiredWWNs is each node's set of desired extra-disk WWNs, normalised for comparison against what a
// domain reports.
func desiredWWNs(specs []provision.NodeSpec) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(specs))
	for _, s := range specs {
		m := make(map[string]bool, len(s.Disks))
		for _, d := range s.Disks {
			m[normalizeWWN(d.WWN)] = true
		}
		out[s.VMName] = m
	}
	return out
}

// moduleDisks reads the module's `extra_disks` output, grouped by node.
func (p *Provisioner) moduleDisks(ctx context.Context, ws, clusterID string) (map[string][]tfExtraDisk, error) {
	out, err := p.run.OutputJSON(ctx, ws, clusterID)
	if err != nil {
		return nil, err
	}
	var outputs struct {
		ExtraDisks struct {
			Value map[string]tfExtraDisk `json:"value"`
		} `json:"extra_disks"`
	}
	if err := json.Unmarshal(out, &outputs); err != nil {
		return nil, fmt.Errorf("tofu: parse extra_disks output: %w", err)
	}
	byNode := map[string][]tfExtraDisk{}
	for _, d := range outputs.ExtraDisks.Value {
		byNode[d.Node] = append(byNode[d.Node], d)
	}
	return byNode, nil
}

// domainDisks returns the domain's currently-attached EXTRA disks (wwn → target device) and whether
// it is running. The root disk carries no wwn and so is never in the map - which is what makes the
// detach paths unable to touch the boot device.
func (p *Provisioner) domainDisks(ctx context.Context, dom string) (map[string]string, bool, error) {
	// silent: this is a probe whose failure is expected and handled by the caller, and whose stdout
	// is a domain's whole XML - neither belongs in the cluster's event timeline.
	out, err := procstream.Capture(ctx, "", nil, silent, "virsh", "-c", p.cfg.LibvirtURI, "dumpxml", dom)
	if err != nil {
		return nil, false, err
	}
	var d libvirtDomain
	if err := xml.Unmarshal(out, &d); err != nil {
		return nil, false, fmt.Errorf("parse domain xml: %w", err)
	}
	disks := map[string]string{}
	for _, disk := range d.Devices.Disks {
		if disk.WWN == "" {
			continue // the root disk
		}
		disks[normalizeWWN(disk.WWN)] = disk.Target.Dev
	}
	state, err := procstream.Capture(ctx, "", nil, silent, "virsh", "-c", p.cfg.LibvirtURI, "domstate", dom)
	if err != nil {
		return nil, false, err
	}
	return disks, strings.TrimSpace(string(state)) == "running", nil
}

// listClusterDomains returns the libvirt domains belonging to a cluster - every domain whose name
// carries the "<clusterID>-" prefix the module mints. Used by the destroy-time cleanup, which has
// only the cluster ID to work from.
func (p *Provisioner) listClusterDomains(ctx context.Context, clusterID string) ([]string, error) {
	out, err := procstream.Capture(ctx, "", nil, silent, "virsh", "-c", p.cfg.LibvirtURI, "list", "--all", "--name")
	if err != nil {
		return nil, err
	}
	prefix := clusterID + "-"
	var doms []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); strings.HasPrefix(name, prefix) {
			doms = append(doms, name)
		}
	}
	return doms, nil
}

// virsh runs one mutating virsh command against the hypervisor, streaming its output into the
// cluster timeline. The change is always made persistent (it must survive a VM reboot) and
// additionally applied live when the domain is running - so a disk appears on, or disappears from, a
// running node without restarting it.
func (p *Provisioner) virsh(ctx context.Context, clusterID string, running bool, args ...string) error {
	full := append([]string{"-c", p.cfg.LibvirtURI}, args...)
	full = append(full, "--persistent")
	if running {
		full = append(full, "--live")
	}
	return procstream.Run(ctx, "", nil, p.run.EmitFor(clusterID), "virsh", full...)
}

// freeTarget picks a SCSI target device name not already in use. The root disk is virtio (vda), so
// the whole sdX range is free; taken is the wwn→target map of what is already attached.
func freeTarget(taken map[string]string) string {
	used := map[string]bool{}
	for _, t := range taken {
		used[t] = true
	}
	for c := byte('a'); c <= 'z'; c++ {
		if t := "sd" + string(c); !used[t] {
			return t
		}
	}
	return "sdz" // unreachable: domain.MaxDisksPerNode is far below 26
}

// normalizeWWN makes the two spellings comparable: libvirt reports a wwn back from the domain XML
// without the 0x prefix the attach was given.
func normalizeWWN(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
}

// domainNameFor is the libvirt domain backing a node. The module composes it as
// "<cluster_name>-<node name>", and it passes the cluster ID as cluster_name (see vars).
func domainNameFor(clusterID, vmName string) string { return clusterID + "-" + vmName }

// silent discards a command's streamed output. procstream calls emit unconditionally, so a nil
// callback would panic.
func silent(string) {}
