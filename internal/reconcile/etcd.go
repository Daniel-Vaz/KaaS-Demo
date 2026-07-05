package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// reconcileEtcd is the Ready-tick half of automatic etcd maintenance, and the exact counterpart of
// reconcileCerts: observe first, act only if the observation says so. It runs for a Ready, converged
// cluster whose etcd status is stale (that's what put it in the work set), re-reads the backend
// store, and promotes into PhaseDefragmentingEtcd only when the policy agrees.
//
// Splitting observe from defragment is what keeps the cheap, always-safe read out of the disruptive
// step - and it means the fleet-wide picture (sizes, fragmentation, armed alarms, the health check
// built on them) exists whether or not defragmentation ever fires.
func (r *Reconciler) reconcileEtcd(ctx context.Context, c *domain.Cluster) error {
	st, err := r.Cfg.EtcdStatus(ctx, c)
	if err != nil {
		return fmt.Errorf("observe etcd status: %w", err)
	}
	// Carry the last defragmentation forward: it is the platform's own record, not something an
	// observation of etcd can tell us, and it is the hysteresis floor that stops a cluster whose
	// keyspace is genuinely large from defragmenting on every pass.
	if c.Etcd != nil {
		st.DefraggedAt = c.Etcd.DefraggedAt
	}
	first := c.Etcd == nil
	c.Etcd = &st
	if first {
		r.emit(c.ID, "info", "reconciler", fmt.Sprintf("observed etcd backend store: %s (%.0f%% fragmented, %.0f%% of quota)",
			domain.HumanBytes(st.DBBytes), st.FragmentationRatio()*100, st.QuotaUsage()*100))
	}

	due, reason := r.EtcdPolicy.DefragDue(st, c.ControlPlanes, time.Now())
	if !due {
		// Deliberately not an event: this runs on every observation interval for every cluster
		// forever, and "still 12% fragmented" is log material, not timeline material.
		r.Log.Debug("etcd maintenance not due", "cluster", c.Name, "reason", reason)
		return nil
	}
	r.emit(c.ID, "info", "reconciler", reason+" - defragmenting")
	c.Phase = domain.PhaseDefragmentingEtcd
	return nil
}

// reconcileDefrag is PhaseDefragmentingEtcd: rebuild every member's etcd backend file to reclaim the
// space compaction freed logically - one member at a time, behind an all-members-healthy pre-flight
// (see etcd-defrag.yml). The playbook returns the post-defrag status, so the reclaimed size is
// stamped without a second observation. Idempotent and resumable: a member already below the
// fragmentation threshold is skipped, so a run killed by the job timeout resumes instead of
// re-bouncing members.
func (r *Reconciler) reconcileDefrag(ctx context.Context, c *domain.Cluster) (err error) {
	// One action-history entry per defragmentation, closed once below with what it reclaimed (the
	// sentence that justifies the brief outage a sole-control-plane defrag causes) or the error.
	opID := r.recordAutoOp(c.ID, domain.OpDefrag, "Automatic etcd defragmentation", "")
	var detail string
	defer func() {
		if err != nil {
			detail = "failed: " + err.Error()
		}
		r.completeAutoOp(opID, detail)
	}()

	before := c.Etcd
	st, err := r.Cfg.DefragEtcd(ctx, c, r.EtcdPolicy.MinRatio)
	if err != nil {
		return err
	}
	c.Etcd = &st
	detail = etcdDefragSummary(before, st)
	r.emit(c.ID, "info", "ansible", detail)
	c.Phase = domain.PhaseReady
	return nil
}

// etcdDefragSummary renders what a defragmentation actually reclaimed, for the cluster's timeline.
// Reporting the delta rather than the new size is the point: "reclaimed 340MiB" is the sentence that
// justifies the outage the user just experienced.
func etcdDefragSummary(before *domain.EtcdStatus, after domain.EtcdStatus) string {
	if before == nil || before.DBBytes <= after.DBBytes {
		return fmt.Sprintf("etcd defragmented - backend store now %s", domain.HumanBytes(after.DBBytes))
	}
	return fmt.Sprintf("etcd defragmented - reclaimed %s, backend store now %s",
		domain.HumanBytes(before.DBBytes-after.DBBytes), domain.HumanBytes(after.DBBytes))
}
