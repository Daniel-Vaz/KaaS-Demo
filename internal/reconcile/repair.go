package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// reconcileRepair is the Ready-tick half of automatic repair: observe, decide, and promote into
// PhaseRepairing only if the policy agrees. The same observe/act split reconcileEtcd and
// reconcileCerts have, and for the same reason - except that here the observation is nearly free
// (it reads a health snapshot the health sweep already stored, plus one power poll), which is why
// the cadence is minutes rather than hours.
//
// Nothing here talks to the cluster. That is deliberate: the cluster being unreachable is one of the
// states this runs in, and a decision procedure that needs the broken thing to answer questions
// cannot decide anything about it.
func (r *Reconciler) reconcileRepair(ctx context.Context, c *domain.Cluster) error {
	now := time.Now()
	obs := domain.RepairObservation{Now: now}

	// The cluster's own view. ErrNotFound is not an error: a cluster whose health has never been
	// evaluated is simply not observable yet, which the policy already knows how to treat.
	if snap, err := r.Store.GetHealth(c.ID); err == nil {
		obs.Health = snap
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("read health snapshot: %w", err)
	}

	// The infrastructure's view - the independent second opinion, and the only signal that survives
	// the cluster's API server being unreachable. Absent on backends without the capability, which
	// the policy distinguishes from "everything is powered on".
	if powerer, ok := provision.AsNodePowerer(r.prov(c)); ok {
		power, err := powerer.NodePower(ctx, c.ID)
		if err != nil {
			// Not fatal. Losing the second opinion narrows what repair may conclude; it does not
			// invalidate the health snapshot, and failing the step would just stop observing.
			r.Log.Debug("repair: power poll", "cluster", c.Name, "err", err)
		} else {
			obs.Powered = power
		}
	}

	fleetFrac, fleetN, err := r.fleetUnhealthy()
	if err != nil {
		return fmt.Errorf("assess fleet health: %w", err)
	}
	obs.FleetUnhealthy, obs.FleetUnhealthyN = fleetFrac, fleetN

	rs := c.RepairState()
	faults, observable := domain.ObserveFaults(c, obs, r.RepairPolicy)
	domain.MergeFaults(rs, c, faults, r.RepairPolicy, now)
	rs.ObservedAt = now
	r.emitFaultChanges(c, rs)

	plan, reason := r.RepairPolicy.Plan(c, obs, faults, observable)
	if !plan.Act() {
		// Deliberately not an event: this runs for every cluster on every observation interval
		// forever, and "all nodes healthy" is log material, not timeline material.
		r.Log.Debug("no repair due", "cluster", c.Name, "reason", reason)
		return nil
	}
	// The attempt is stamped BEFORE the work, which is the whole reason Target/Action are persisted
	// rather than re-derived in PhaseRepairing. A crash between here and the repair leaves an
	// incremented counter and a running backoff - which is correct: the platform did start a repair,
	// and a counter that only advances on success is a counter that never gives up.
	rs.RecordAttempt(plan, r.RepairPolicy, now)
	r.emit(c.ID, "warn", "reconciler", "auto-repair: "+reason)
	c.Phase = domain.PhaseRepairing
	return nil
}

// emitFaultChanges puts fault transitions on the cluster's timeline - appearing, clearing, and being
// given up on - and nothing else. A steady-state fault is not re-announced every observation
// interval: the timeline is for changes, and a feed that repeats itself is one nobody reads.
func (r *Reconciler) emitFaultChanges(c *domain.Cluster, rs *domain.ClusterRepair) {
	for vm, st := range rs.Nodes {
		switch {
		case st.Suspended && st.Note != "":
			// Suspension is announced once, when RecordAttempt sets it; the note is cleared below so
			// a still-suspended node does not re-announce on every pass.
			r.emit(c.ID, "error", "reconciler", "auto-repair: "+st.Note)
			st.Note = ""
		case st.Fault != "" && st.Attempts == 0 && st.UnhealthySince != nil && st.UnhealthySince.Equal(rs.ObservedAt):
			r.emit(c.ID, "warn", "reconciler", fmt.Sprintf("node %s is %s", vm, st.Fault))
		case st.Fault == "" && st.RepairedAt != nil && st.RepairedAt.Equal(rs.ObservedAt) && st.Attempts > 0:
			r.emit(c.ID, "info", "reconciler", fmt.Sprintf("node %s recovered after %d repair attempt(s)", vm, st.Attempts))
		}
	}
}

// fleetUnhealthy is the share of the platform's live clusters currently reporting unhealthy.
// It feeds the circuit breaker that no per-cluster guard can replace: when the worker loses the
// hypervisor or the tunnel, every cluster goes unhealthy at once and each one looks, locally, like an
// ordinary repairable fault. This is the number that says otherwise.
//
// Counted from stored health snapshots rather than by probing anything, so it costs one query and is
// available even when nothing is reachable - which is precisely when it matters.
func (r *Reconciler) fleetUnhealthy() (float64, int, error) {
	clusters, err := r.Store.ListClusters()
	if err != nil {
		return 0, 0, err
	}
	live, unhealthy := 0, 0
	for _, c := range clusters {
		if c.Phase != domain.PhaseReady {
			continue
		}
		live++
		snap, err := r.Store.GetHealth(c.ID)
		if err != nil || snap == nil {
			continue // never evaluated: unknown, not unhealthy
		}
		if snap.Status == domain.HealthUnhealthy {
			unhealthy++
		}
	}
	if live == 0 {
		return 0, 0, nil
	}
	return float64(unhealthy) / float64(live), unhealthy, nil
}

// executeRepair performs the single action the Ready tick decided on. One rung, one node, one
// invocation - the same one-step-per-tick shape as rollOneNode, and for the same reason: each step
// is separately retryable and the progress is visible.
func (r *Reconciler) executeRepair(ctx context.Context, c *domain.Cluster) error {
	rs := c.RepairState()
	if !rs.InFlight() {
		return nil // nothing decided (a re-run after the plan was completed); harmless
	}
	node, ok := nodeByNameOK(c.Nodes, rs.Target)
	if !ok {
		// The node vanished from desired state between the decision and now - a scale-down landed.
		// Not an error: the fault is moot, and MergeFaults will drop its state on the next pass.
		return nil
	}
	// Record this rung in the cluster's action history. A restore is called out with its own kind -
	// it is the one lossy repair and worth a distinct line - everything else is an OpRepair. The op
	// is closed as soon as the action returns: "completed" here means the platform finished ATTEMPTING
	// the rung, not that the node is fixed (that is decided by the next observation - see
	// CompleteAttempt), so a failed action is completed with its error rather than left dangling.
	kind := domain.OpRepair
	if rs.Action == domain.ActionRestore {
		kind = domain.OpRestore
	}
	opID := r.recordAutoOp(c.ID, kind, repairOpSummary(rs.Action, node.VMName), r.repairOpDetail(rs))
	err := r.runRepairAction(ctx, c, rs, node)
	if err != nil {
		r.completeAutoOp(opID, "failed: "+err.Error())
	} else {
		r.completeAutoOp(opID, "") // keep the recorded fault/attempt detail
	}
	return err
}

// runRepairAction performs the single rung the Ready tick decided on. Extracted from executeRepair so
// the action-history bookkeeping wraps it uniformly, whichever rung it is.
func (r *Reconciler) runRepairAction(ctx context.Context, c *domain.Cluster, rs *domain.ClusterRepair, node domain.Node) error {
	switch rs.Action {
	case domain.ActionPowerOn:
		powerer, ok := provision.AsNodePowerer(r.prov(c))
		if !ok {
			return fmt.Errorf("provider %s cannot power on a VM", c.InfraProvider())
		}
		r.emit(c.ID, "info", "infra", fmt.Sprintf("auto-repair: starting VM %s", node.VMName))
		return powerer.PowerOnNode(ctx, c.ID, node.VMName)

	case domain.ActionRestartKubelet:
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("auto-repair: restarting kubelet on %s", node.VMName))
		return r.Cfg.RestartKubelet(ctx, c, node)

	case domain.ActionRejoin:
		// The idempotent join, exactly as scale-up runs it: nodes already in the cluster are skipped,
		// so this costs nothing for the healthy ones and joins the one that never made it.
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("auto-repair: re-running join for %s", node.VMName))
		if node.Role == domain.RoleControlPlane {
			return r.Cfg.JoinControlPlane(ctx, c, node)
		}
		return r.Cfg.JoinWorkers(ctx, c)

	case domain.ActionReplace:
		return r.repairByReplacement(ctx, c, node)

	case domain.ActionRestore:
		return r.repairByRestore(ctx, c, node)
	}
	return fmt.Errorf("unknown repair action %q", rs.Action)
}

// repairByReplacement rebuilds a faulty node onto the SAME golden image it is already running.
//
// It is the rolling-replacement machinery pointed at a different question. An OS upgrade replaces a
// node because its image changed; a repair replaces one because the node is broken and the image is
// fine - so the only difference at this level is which image the rebuilt node gets, and both go
// through the same replaceNode. That is what keeps automatic repair from inventing a second, less
// well-tested way to destroy and rebuild a node.
func (r *Reconciler) repairByReplacement(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	image := node.Image
	if image == "" {
		// A node row predating per-node image tracking. Fall back to the cluster's current bundle
		// image rather than replacing onto "" - which the provisioner would read as the module's
		// default base image and could silently change the node's OS during what is meant to be a
		// like-for-like rebuild.
		image = r.currentImage(c)
	}
	r.emit(c.ID, "warn", "reconciler", fmt.Sprintf("auto-repair: rebuilding %s (its root disk is replaced; extra disks are kept)", node.VMName))
	return r.replaceNode(ctx, c, node, image, true)
}

// repairByRestore rebuilds a SOLE control plane from a stored snapshot: the platform's only lossy
// repair, reached only when there is no live control plane left to copy state from.
func (r *Reconciler) repairByRestore(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	if c.HA() {
		// Defensive: destructiveAction only ever chooses this for a sole control plane, and an HA
		// cluster restoring one member's snapshot over a live quorum would be a catastrophe rather
		// than a repair.
		return fmt.Errorf("refusing to restore a snapshot onto an HA cluster - %s should be replaced instead", node.VMName)
	}
	image := node.Image
	if image == "" {
		image = r.currentImage(c)
	}
	// Rebuild the VM first, then put the state back on it. The node reclaims its IP via its pinned
	// MAC, which is what makes the snapshot's certs and etcd member identity still valid.
	//
	// Deliberately NOT through replaceNode: that path drains the node out of the cluster first, and
	// there is no cluster left to drain it from - the sole control plane is the thing that is gone.
	// This is the one replacement that starts from nothing.
	if _, err := r.recreateNodeVM(ctx, c, node, image, true); err != nil {
		return err
	}
	return r.restoreSnapshot(ctx, c, nodeByName(c.Nodes, node.VMName))
}

// currentImage is the golden image this cluster's bundle currently pins, used only as a fallback for
// a node row that predates per-node image tracking.
func (r *Reconciler) currentImage(c *domain.Cluster) string {
	return catalog.GoldenImageNameFor(c.InfraProvider(), c.OSImage, c.K8sVersion)
}

// nodeByNameOK is nodeByName with a found flag, for the callers that must distinguish "not there"
// from "zero value".
func nodeByNameOK(nodes []domain.Node, name string) (domain.Node, bool) {
	for _, n := range nodes {
		if n.VMName == name {
			return n, true
		}
	}
	return domain.Node{}, false
}
