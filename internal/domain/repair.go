package domain

import (
	"fmt"
	"sort"
	"time"
)

// NodeFault is what the platform believes is wrong with one node. The set is deliberately small:
// each value maps to a different CHEAPEST repair, and a distinction that does not change the action
// is not a fault class, it is a detail for the summary line.
type NodeFault string

const (
	// FaultNotReady: the Kubernetes node object exists and reports Ready=false. The kubelet is
	// registered but not serving - hung, crashed, out of disk, or its container runtime is wedged.
	FaultNotReady NodeFault = "notready"
	// FaultMissing: the platform has a VM for this node, but the cluster has no node object for it.
	// Either it never joined (a failed bootstrap token, a cloud-init that did not finish) or
	// something deleted it. Cheapest repair is to re-run the idempotent join, not to rebuild.
	FaultMissing NodeFault = "missing"
	// FaultVMDown: the INFRASTRUCTURE says the VM is not running. This is the only fault observed
	// below the Kubernetes layer, and that independence is what makes it load-bearing - see
	// RepairPolicy.Plan, where it is the one fault the platform will still act on when the cluster's
	// own API server is unreachable and every other signal is therefore untrustworthy.
	FaultVMDown NodeFault = "vm-down"
)

// RepairAction is one rung of the escalation ladder, cheapest first. Every action is idempotent and
// every one of them is an operation the platform already performs for another reason - which is the
// point: automatic repair adds decision-making, not new ways to touch a cluster.
type RepairAction string

const (
	// ActionPowerOn starts a VM the hypervisor reports as off. Seconds, and non-destructive.
	ActionPowerOn RepairAction = "power-on"
	// ActionRejoin re-runs the idempotent join for a node that has a VM but no node object.
	ActionRejoin RepairAction = "rejoin"
	// ActionRestartKubelet restarts kubelet (and containerd under it) over SSH. Seconds, and the
	// repair for the large majority of NotReady nodes.
	ActionRestartKubelet RepairAction = "restart-kubelet"
	// ActionReplace destroys the node's root volume and domain and rebuilds it from the SAME golden
	// image it was already running, then rejoins it. Minutes, destructive to anything on the root
	// disk, and non-destructive to the node's extra disks (see reconcile.replaceNode).
	ActionReplace RepairAction = "replace"
	// ActionRestore rebuilds a SOLE control plane from a stored etcd snapshot. The only lossy
	// action the platform takes on its own, reserved for the case where there is no live control
	// plane left to copy state from - see EtcdSnapshot.
	ActionRestore RepairAction = "restore"
)

// NodeRepairState is the platform's own durable memory about one node's health and what it has
// already tried. It exists because HealthSnapshot is point-in-time and overwritten on every sweep
// (store.SaveHealth keeps only the newest), while every safe repair decision is a statement about
// DURATION - "NotReady for twenty minutes", not "NotReady right now". Without this there is nowhere
// to read the sentence from.
type NodeRepairState struct {
	// Fault is what is currently believed wrong; empty once the node recovers.
	Fault NodeFault `json:"fault,omitempty"`
	// UnhealthySince is the first observation that saw this fault. The clock every grace period and
	// escalation threshold is measured from.
	UnhealthySince *time.Time `json:"unhealthy_since,omitempty"`
	// Attempts is how many repairs have been started for this fault episode. It is the rung index
	// AND the give-up counter, and it is incremented BEFORE the work runs - see RecordAttempt.
	Attempts int `json:"attempts,omitempty"`
	// LastAction / LastActionAt are what was tried and when; together with Attempts they drive the
	// exponential backoff that keeps a doomed node from being rebuilt on a loop.
	LastAction   RepairAction `json:"last_action,omitempty"`
	LastActionAt *time.Time   `json:"last_action_at,omitempty"`
	// RepairedAt is when this node last came back healthy after the platform acted on it. Kept
	// AFTER recovery (briefly) so a node that fails again immediately is recognised as flapping
	// rather than starting the ladder over - recovery would otherwise be an unlimited supply of
	// fresh attempts, which is the hole in "give up loudly".
	RepairedAt *time.Time `json:"repaired_at,omitempty"`
	// Suspended means the platform has given up on this node and will not touch it again until it
	// recovers on its own or a human intervenes. A repair loop is worse than the fault it chases:
	// it burns host capacity and, by keeping the cluster in perpetual motion, hides the real cause.
	Suspended bool `json:"suspended,omitempty"`
	// Note is the human-readable reason for the current state, surfaced in the portal.
	Note string `json:"note,omitempty"`
}

// ClusterRepair is the whole repair state carried on the cluster row (one JSONB column), including
// the repair currently IN FLIGHT.
//
// Target/Action are persisted rather than re-derived because the reconciler advances one phase per
// invocation: the Ready tick decides, PhaseRepairing executes, and those are two different jobs on
// possibly two different worker replicas. Re-deriving the plan in the second one would re-run the
// policy against an Attempts counter the first one already incremented, and land on a different
// rung than the one that was announced.
type ClusterRepair struct {
	Nodes map[string]*NodeRepairState `json:"nodes,omitempty"`
	// Target is the VM name PhaseRepairing is about to act on; Action is what it will do.
	Target string       `json:"target,omitempty"`
	Action RepairAction `json:"action,omitempty"`
	// ObservedAt is when the repair state was last refreshed, driving the re-observation cadence.
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// RepairState returns the cluster's repair state, creating it if this is the first time anything
// has asked. Every reader goes through it so a nil Repair (every cluster predating this feature) is
// never a panic waiting in a policy function.
func (c *Cluster) RepairState() *ClusterRepair {
	if c.Repair == nil {
		c.Repair = &ClusterRepair{}
	}
	return c.Repair
}

// State returns the state for vm, creating it if absent.
func (r *ClusterRepair) State(vm string) *NodeRepairState {
	if r.Nodes == nil {
		r.Nodes = map[string]*NodeRepairState{}
	}
	if r.Nodes[vm] == nil {
		r.Nodes[vm] = &NodeRepairState{}
	}
	return r.Nodes[vm]
}

// Unhealthy counts the nodes currently believed faulty.
func (r *ClusterRepair) Unhealthy() int {
	n := 0
	for _, s := range r.Nodes {
		if s.Fault != "" {
			n++
		}
	}
	return n
}

// Suspended lists the nodes the platform has given up on, for the health check.
func (r *ClusterRepair) SuspendedNodes() []string {
	var out []string
	for vm, s := range r.Nodes {
		if s.Suspended {
			out = append(out, vm)
		}
	}
	sort.Strings(out)
	return out
}

// InFlight reports whether a repair has been decided and not yet completed.
func (r *ClusterRepair) InFlight() bool { return r.Target != "" && r.Action != "" }

// RepairPolicy decides WHETHER, WHEN and HOW the platform repairs a node. Like EtcdDefragPolicy it
// is a plain value, so every one of the guards below is unit-tested without a cluster, an ansible
// run, or a clock - which matters more here than anywhere else in the platform, because this is the
// one policy whose failure mode is destroying working infrastructure.
type RepairPolicy struct {
	// Enabled turns the whole feature on, observation included.
	Enabled bool
	// Replace gates the destructive rung on its own, so an operator can keep power-on, rejoin and
	// kubelet-restart while forbidding the platform to rebuild a node unattended.
	Replace bool
	// Restore gates the lossy rung - rebuilding a sole control plane from a stored snapshot - on its
	// own, for the same reason and more so.
	Restore bool
	// ObserveInterval is how often a Ready cluster's repair state is refreshed. Fast relative to the
	// other periodic work, because the observation is free: it reads a health snapshot the health
	// sweep already stored, plus one power-state call.
	ObserveInterval time.Duration
	// HealthMaxAge is how stale the health snapshot may be and still be believed. Past it the
	// cluster layer is treated as UNOBSERVABLE rather than as reporting nothing wrong - see Plan.
	HealthMaxAge time.Duration
	// NotReadyGrace is how long a node must stay NotReady before the first repair. Kubernetes marks
	// a node NotReady 40s after its kubelet stops reporting, and kubelets miss heartbeats for
	// entirely transient reasons; acting on the first reading is how a platform rebuilds a node that
	// was about to come back on its own.
	NotReadyGrace time.Duration
	// NodeStartupGrace is how long a node with no node object is allowed to be joining before it
	// counts as faulty. Without it repair fights provisioning: every freshly created node is
	// "missing" for the minutes between its VM booting and kubeadm join returning.
	NodeStartupGrace time.Duration
	// ReplaceAfter is how long a fault must persist, across cheaper attempts, before the node is
	// rebuilt.
	ReplaceAfter time.Duration
	// MaxUnhealthyFraction is the share of a cluster's nodes that may be faulty before repair stands
	// down for that cluster entirely. Above it, this is not N node faults - it is one cluster-wide
	// or network-wide fault wearing N masks, and rebuilding nodes makes it worse. Cluster API's
	// MachineHealthCheck short-circuits on the same reasoning (maxUnhealthy).
	MaxUnhealthyFraction float64
	// MaxUnhealthyClusters is the same guard one level up, across the FLEET. No per-cluster check
	// can catch the case this exists for: when the worker loses the hypervisor or the tunnel, every
	// cluster goes unhealthy at once and each one looks, locally, like an ordinary repairable fault.
	// This is the guard that stands between a dropped VPN and a rebuilt estate.
	MaxUnhealthyClusters float64
	// MaxAttempts is how many repairs one fault episode gets before the node is suspended.
	MaxAttempts int
	// Backoff is the base delay between attempts; it doubles per attempt.
	Backoff time.Duration
}

// RepairObservation is everything the policy is allowed to look at. Gathering it is the reconciler's
// job (it needs the store and the provisioner); deciding on it is this file's, and the split is what
// keeps the decision pure.
type RepairObservation struct {
	// Health is the newest stored health snapshot, or nil if none has ever been taken.
	Health *HealthSnapshot
	// Powered maps VM name to whether the infrastructure reports the VM as running. A nil map means
	// the provisioner cannot answer (fake mode, or a backend without the capability) - which is
	// different from a map saying everything is up, and is treated differently.
	Powered map[string]bool
	// FleetUnhealthy is the share of the platform's live clusters currently reporting unhealthy,
	// including this one, and FleetUnhealthyN the count behind it. Both feed the fleet-wide circuit
	// breaker: the fraction decides whether the outage is broad, the COUNT whether "broad" means
	// anything yet - one unhealthy cluster is 100% of a one-cluster fleet, which is a cluster fault,
	// not a fleet fault (the exact mirror of the per-cluster guard needing at least two faults).
	FleetUnhealthy  float64
	FleetUnhealthyN int
	Now             time.Time
}

// RepairPlan is the single action the policy has decided on, or the zero value for "do nothing".
type RepairPlan struct {
	Target string
	Action RepairAction
	Fault  NodeFault
}

// Act reports whether the plan calls for anything.
func (p RepairPlan) Act() bool { return p.Target != "" && p.Action != "" }

// RepairObservationDue reports whether the cluster's repair state is stale enough to refresh.
func (p RepairPolicy) RepairObservationDue(r *ClusterRepair, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if r == nil || r.ObservedAt.IsZero() {
		return true
	}
	return now.Sub(r.ObservedAt) >= p.ObserveInterval
}

// RepairDue reports whether the reconciler has repair work for this cluster now. Narrow in exactly
// the way CertRenewalDue and EtcdMaintenanceDue are - only a Ready, converged cluster qualifies - so
// repair never races an in-flight transition. That guard is doing more work here than for the other
// two: during an update or an upgrade, nodes are drained, removed and rebuilt on purpose, and every
// one of them looks exactly like the faults this policy repairs.
func (c *Cluster) RepairDue(p RepairPolicy, now time.Time) bool {
	if c.Phase != PhaseReady || c.ObservedGeneration != c.Generation {
		return false
	}
	return p.RepairObservationDue(c.Repair, now)
}

// ObserveFaults derives the current per-node faults from an observation, and reports whether the
// cluster was observable at all.
//
// The observable flag is the most important thing this function returns. When the API server is
// unreachable - or the health snapshot is too old to believe - every node reads NotReady, and the
// honest conclusion is "I cannot see this cluster", not "this cluster is entirely broken". Acting on
// the second reading is how a platform destroys a healthy cluster during a network partition.
//
// There is exactly one exception, and it is the reason the infrastructure layer is consulted at all:
// a VM the HYPERVISOR reports as powered off is a fault observed BELOW the thing that is blind, from
// an independent source. That corroboration is what makes control-plane repair possible - on a
// single-control-plane cluster the API server going away is precisely the symptom, so a policy that
// refuses to act whenever the API server is unreachable could never repair the case it exists for.
func ObserveFaults(c *Cluster, obs RepairObservation, p RepairPolicy) (faults map[string]NodeFault, observable bool) {
	faults = map[string]NodeFault{}

	// The infrastructure layer first: it is independent of the cluster's own health, so its findings
	// stand whether or not the API server answers.
	for _, n := range c.Nodes {
		if up, known := obs.Powered[n.VMName]; known && !up {
			faults[n.VMName] = FaultVMDown
		}
	}

	observable = obs.Health != nil &&
		obs.Now.Sub(obs.Health.CollectedAt) <= p.HealthMaxAge &&
		!apiServerUnreachable(obs.Health)
	if !observable {
		return faults, false
	}

	reported := make(map[string]bool, len(obs.Health.Nodes))
	for _, nh := range obs.Health.Nodes {
		reported[nh.NodeName] = true
		if !nh.Ready {
			if _, already := faults[nh.NodeName]; !already {
				faults[nh.NodeName] = FaultNotReady
			}
		}
	}
	// A node the platform has a VM for but the cluster has never heard of. Skipped when the VM has no
	// IP yet - that node is still being provisioned, not missing.
	for _, n := range c.Nodes {
		if n.IP == "" || reported[n.VMName] {
			continue
		}
		if _, already := faults[n.VMName]; !already {
			faults[n.VMName] = FaultMissing
		}
	}
	return faults, true
}

// apiServerUnreachable reports whether the snapshot's API-server check failed, which invalidates
// every per-node reading in it.
func apiServerUnreachable(s *HealthSnapshot) bool {
	for _, ch := range s.Checks {
		if ch.ID == "api-server" {
			return ch.Status == HealthUnhealthy
		}
	}
	// No API-server check at all is not a pass: it means this snapshot was produced by something
	// that did not evaluate the one signal every other reading depends on.
	return true
}

// MergeFaults folds a fresh set of observed faults into the cluster's durable repair state: it
// stamps UnhealthySince on a newly faulty node, clears a recovered one, and drops nodes that no
// longer exist. Pure, and the only writer of UnhealthySince.
func MergeFaults(r *ClusterRepair, c *Cluster, faults map[string]NodeFault, p RepairPolicy, now time.Time) {
	if r.Nodes == nil {
		r.Nodes = map[string]*NodeRepairState{}
	}
	live := make(map[string]bool, len(c.Nodes))
	for _, n := range c.Nodes {
		live[n.VMName] = true
	}
	// Drop state for nodes that are no longer desired at all (a pool scaled down, a cluster shrunk).
	// Stale entries here are state nothing converges - the same reasoning that prunes node disks off
	// departing nodes.
	for vm := range r.Nodes {
		if !live[vm] {
			delete(r.Nodes, vm)
		}
	}
	for vm := range live {
		fault, faulty := faults[vm]
		st := r.State(vm)
		switch {
		case !faulty && st.Fault == "":
			// Healthy and was healthy. Let a stale RepairedAt age out so the flap window does not
			// follow a node around forever.
			if st.RepairedAt != nil && now.Sub(*st.RepairedAt) > p.flapWindow() {
				delete(r.Nodes, vm)
			}
		case !faulty:
			// Recovered. Keep a breadcrumb rather than forgetting entirely: an episode that ends
			// right after the platform acted, and then reopens, is a flapping node, and starting the
			// ladder from rung zero every time would be an unlimited supply of fresh attempts.
			repaired := now
			was := st.Attempts
			*st = NodeRepairState{RepairedAt: &repaired, Attempts: was}
			if was > 0 {
				st.Note = fmt.Sprintf("recovered after %d repair attempt(s)", was)
			}
		case st.Fault == fault && st.UnhealthySince != nil:
			// Same fault continuing; leave the clock alone. This is the whole point of the type.
		default:
			// A new episode, or the fault changed class (NotReady became VM-down). Reset the clock -
			// but carry attempts forward when the node is flapping, so repeated short episodes
			// escalate and eventually suspend instead of resetting each other.
			since := now
			carried := 0
			if st.RepairedAt != nil && now.Sub(*st.RepairedAt) <= p.flapWindow() {
				carried = st.Attempts
			}
			*st = NodeRepairState{Fault: fault, UnhealthySince: &since, Attempts: carried}
			if carried > 0 {
				st.Note = fmt.Sprintf("re-failed within %s of a repair", HumanDuration(p.flapWindow()))
			}
		}
	}
}

// flapWindow is how soon after a repair a fresh fault counts as the same episode continuing rather
// than a new one. Derived from the backoff rather than configured separately: the question it
// answers ("did this node really recover, or did it just pause?") has the same natural scale as the
// question of how long to wait between attempts.
func (p RepairPolicy) flapWindow() time.Duration {
	if p.Backoff <= 0 {
		return time.Hour
	}
	return 4 * p.Backoff
}

// Plan decides the ONE repair to perform now, and explains itself either way. The reason is returned
// for both answers because "why has the platform not fixed my node" is the question an operator
// actually asks, and it is unanswerable from a boolean.
//
// One node at a time, per cluster, always. Repairing two nodes concurrently doubles the blast radius
// of a wrong decision and, on a cluster whose real problem is shared (a full datastore, a bad
// image), converts a fault into an outage.
func (p RepairPolicy) Plan(c *Cluster, obs RepairObservation, faults map[string]NodeFault, observable bool) (RepairPlan, string) {
	if !p.Enabled {
		return RepairPlan{}, "automatic repair is disabled"
	}
	// The fleet-wide breaker comes first, before anything about this cluster is even considered: when
	// it trips, everything about this cluster is suspect, including the evidence. It needs at least
	// TWO unhealthy clusters to trip - a single one is 100% of a one-cluster fleet, and inferring a
	// platform-wide fault from a sample of one would make automatic repair a no-op on every small
	// deployment. Same shape as the per-cluster guard's two-fault minimum below.
	if p.MaxUnhealthyClusters > 0 && obs.FleetUnhealthyN >= 2 && obs.FleetUnhealthy > p.MaxUnhealthyClusters {
		return RepairPlan{}, fmt.Sprintf("standing down - %d clusters (%.0f%% of the fleet) are unhealthy (limit %.0f%%), which is a platform-level fault, not %d node faults",
			obs.FleetUnhealthyN, obs.FleetUnhealthy*100, p.MaxUnhealthyClusters*100, len(faults))
	}
	if len(faults) == 0 {
		if !observable {
			return RepairPlan{}, "cluster is not observable (API server unreachable or health stale) and no VM is reported down"
		}
		return RepairPlan{}, "all nodes healthy"
	}
	// The blast-radius guard needs AT LEAST TWO faults to mean anything. Its whole premise is that a
	// simultaneous fault on many nodes is one shared cause wearing N masks - and a single fault has
	// no N to infer that from, however large a share of the cluster it happens to be. Without this
	// clause a sole-control-plane cluster is 100% faulty the moment its one node breaks and could
	// therefore never be repaired: the exact case the whole snapshot-and-restore path exists for.
	if total := len(c.Nodes); total > 0 && p.MaxUnhealthyFraction > 0 && len(faults) > 1 {
		if frac := float64(len(faults)) / float64(total); frac > p.MaxUnhealthyFraction {
			return RepairPlan{}, fmt.Sprintf("standing down - %d/%d nodes faulty (limit %.0f%%), which is a cluster-wide fault rather than a node fault",
				len(faults), total, p.MaxUnhealthyFraction*100)
		}
	}

	// Workers before control planes, the same order rolling replacement uses and for the same
	// reason: control planes are the riskiest thing to touch, so they are touched last. It needs no
	// special case for "the API server is down so only a control-plane repair can help" - in that
	// state the worker faults are not observable, so there are none to come first.
	var skipped string
	for _, n := range append(workersOf(c), controlPlanesOf(c)...) {
		fault, faulty := faults[n.VMName]
		if !faulty {
			continue
		}
		st := c.RepairState().State(n.VMName)
		action, why := p.nextAction(c, n, st, fault, obs.Now)
		if action == "" {
			if skipped == "" {
				skipped = fmt.Sprintf("%s: %s", n.VMName, why)
			}
			continue
		}
		return RepairPlan{Target: n.VMName, Action: action, Fault: fault}, fmt.Sprintf("%s is %s - %s", n.VMName, fault, why)
	}
	if skipped == "" {
		skipped = "no actionable fault"
	}
	return RepairPlan{}, skipped
}

// nextAction picks the rung for one faulty node, or "" with the reason it is being left alone.
func (p RepairPolicy) nextAction(c *Cluster, n Node, st *NodeRepairState, fault NodeFault, now time.Time) (RepairAction, string) {
	if st.Suspended {
		return "", fmt.Sprintf("repair suspended after %d attempt(s)", st.Attempts)
	}
	if st.Attempts >= p.MaxAttempts {
		return "", fmt.Sprintf("repair attempts exhausted (%d)", st.Attempts)
	}
	if st.LastActionAt != nil {
		if wait := p.backoffFor(st.Attempts); now.Sub(*st.LastActionAt) < wait {
			return "", fmt.Sprintf("backing off %s after %s", HumanDuration(wait-now.Sub(*st.LastActionAt)), st.LastAction)
		}
	}
	if st.UnhealthySince == nil {
		return "", "fault not yet timestamped"
	}
	elapsed := now.Sub(*st.UnhealthySince)

	// The cheap rung, chosen by fault class, and only on the first attempt of an episode.
	if st.Attempts == 0 {
		switch fault {
		case FaultVMDown:
			// No grace period: the hypervisor saying a VM is off is not a heartbeat that might come
			// back, and starting it costs seconds and loses nothing.
			return ActionPowerOn, "powering the VM back on"
		case FaultMissing:
			if elapsed < p.NodeStartupGrace {
				return "", fmt.Sprintf("within the %s startup grace (%s so far)", HumanDuration(p.NodeStartupGrace), HumanDuration(elapsed))
			}
			return ActionRejoin, fmt.Sprintf("not in the cluster after %s - re-running join", HumanDuration(elapsed))
		default: // FaultNotReady
			if elapsed < p.NotReadyGrace {
				return "", fmt.Sprintf("within the %s NotReady grace (%s so far)", HumanDuration(p.NotReadyGrace), HumanDuration(elapsed))
			}
			return ActionRestartKubelet, fmt.Sprintf("NotReady for %s - restarting kubelet", HumanDuration(elapsed))
		}
	}

	// Escalation. The cheap rung was tried and the fault outlived it.
	if elapsed < p.ReplaceAfter {
		return "", fmt.Sprintf("%s tried; waiting until %s before replacing (%s so far)", st.LastAction, HumanDuration(p.ReplaceAfter), HumanDuration(elapsed))
	}
	return p.destructiveAction(c, n, elapsed)
}

// destructiveAction picks between rebuilding a node and restoring a sole control plane, and refuses
// both where refusing is the safe answer. Every guard here is about not converting a node fault into
// a cluster loss.
func (p RepairPolicy) destructiveAction(c *Cluster, n Node, elapsed time.Duration) (RepairAction, string) {
	if !p.Replace {
		return "", "node replacement is disabled (KAAS_REPAIR_REPLACE)"
	}
	if n.Role != RoleControlPlane {
		return ActionReplace, fmt.Sprintf("still faulty after %s - rebuilding the node", HumanDuration(elapsed))
	}

	// A SOLE control plane cannot be drained, removed or copied from - there is nowhere to move its
	// etcd member to and nothing left serving. Rebuilding it means putting back a stored snapshot,
	// which is the platform's only lossy action.
	if !c.HA() {
		if !p.Restore {
			return "", "sole control plane is unrecoverable without a snapshot restore, which is disabled (KAAS_REPAIR_RESTORE)"
		}
		return ActionRestore, fmt.Sprintf("sole control plane down for %s - rebuilding from the newest etcd snapshot", HumanDuration(elapsed))
	}

	// HA: replacing a control plane takes its etcd member down for the duration. Doing that while
	// another member is already unreachable is how a three-member cluster loses quorum - and unlike
	// every other condition here, which weighs whether the repair is WORTH doing, this one decides
	// whether it is SAFE. Same refusal, same reason, as EtcdDefragPolicy.DefragDue.
	if c.Etcd != nil && c.Etcd.Members > 0 && c.Etcd.Members < c.ControlPlaneCount() {
		return "", fmt.Sprintf("refusing to replace a control plane while only %d/%d etcd members are reachable - that is how quorum is lost",
			c.Etcd.Members, c.ControlPlaneCount())
	}
	if c.Repair != nil {
		down := 0
		for _, cp := range controlPlanesOf(c) {
			if s := c.Repair.Nodes[cp.VMName]; s != nil && s.Fault != "" {
				down++
			}
		}
		if down > 1 {
			return "", fmt.Sprintf("refusing to replace a control plane while %d of %d are faulty", down, c.ControlPlaneCount())
		}
	}
	return ActionReplace, fmt.Sprintf("still faulty after %s - rebuilding the control plane", HumanDuration(elapsed))
}

// backoffFor is the wait before attempt n+1, doubling per attempt and capped so the exponent cannot
// run away into a delay longer than anyone will wait for.
func (p RepairPolicy) backoffFor(attempts int) time.Duration {
	if p.Backoff <= 0 || attempts <= 0 {
		return 0
	}
	d := p.Backoff
	for i := 1; i < attempts && d < 24*time.Hour; i++ {
		d *= 2
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// RecordAttempt stamps an attempt on the node's state and marks the plan in flight. It is called
// BEFORE the repair runs, never after, and that ordering is load-bearing for the same reason
// drain-before-destroy is: the durable record has to precede the irreversible act. Incrementing
// afterwards means a crash mid-repair leaves a node that is retried forever without ever escalating
// or being suspended - the exact failure this counter exists to prevent.
func (r *ClusterRepair) RecordAttempt(plan RepairPlan, p RepairPolicy, now time.Time) {
	st := r.State(plan.Target)
	st.Attempts++
	st.LastAction = plan.Action
	at := now
	st.LastActionAt = &at
	st.Note = fmt.Sprintf("%s (attempt %d/%d)", plan.Action, st.Attempts, p.MaxAttempts)
	if st.Attempts >= p.MaxAttempts {
		st.Suspended = true
		st.Note = fmt.Sprintf("%s failed to fix %s after %d attempt(s) - repair suspended, needs a human",
			plan.Action, plan.Target, st.Attempts)
	}
	r.Target, r.Action = plan.Target, plan.Action
}

// CompleteAttempt clears the in-flight plan once PhaseRepairing has run. It deliberately does NOT
// clear the fault: whether the repair worked is decided by the next OBSERVATION, not by the action
// returning without an error. A kubelet restart that returns cleanly and changes nothing must not
// look like a success, or the ladder never escalates.
func (r *ClusterRepair) CompleteAttempt() {
	r.Target, r.Action = "", ""
}

// workersOf and controlPlanesOf split a cluster's nodes by role, preserving order.
func workersOf(c *Cluster) []Node {
	var out []Node
	for _, n := range c.Nodes {
		if n.Role != RoleControlPlane {
			out = append(out, n)
		}
	}
	return out
}

func controlPlanesOf(c *Cluster) []Node {
	var out []Node
	for _, n := range c.Nodes {
		if n.Role == RoleControlPlane {
			out = append(out, n)
		}
	}
	return out
}
