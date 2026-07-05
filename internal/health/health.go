// Package health is the cluster-health telemetry seam: evaluate a set of dedicated health checks
// (API server reachable, nodes Ready, core system workloads, scheduling capacity, etcd quorum,
// add-on availability) against a cluster and surface them as an observed status.
//
// Like metrics.Collector this seam is read-only and its output is live/observed telemetry, not
// desired state: the reconciler evaluates it on a slow ticker and upserts the latest snapshot into
// the store, and the API serves that snapshot read-through (only the worker can reach the cluster
// network - see docs/networking.md). Health is deliberately decoupled from the reconcile state
// machine: it never changes a cluster's Phase (a Ready cluster can be Degraded). Fake synthesizes
// an all-healthy snapshot from control-plane state so the portal's health panel renders without a
// real cluster; the real implementation (health/kubectl) queries the cluster API with
// `kubectl get --raw`.
package health

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Stable check IDs the portal keys on. Kept here so the fake, the real checker, and the UI agree.
const (
	CheckAPIServer       = "api-server"
	CheckNodesReady      = "nodes-ready"
	CheckSystemWorkloads = "system-workloads"
	CheckScheduling      = "scheduling-capacity"
	CheckEtcd            = "etcd-quorum"
	CheckEtcdStore       = "etcd-store"
	CheckAddons          = "addon-availability"
	CheckCerts           = "control-plane-certs"
	CheckBackup          = "etcd-backup"
	CheckRepair          = "auto-repair"
)

// Human-readable names paired with the IDs above, so every implementation labels a check the same.
var checkNames = map[string]string{
	CheckAPIServer:       "API server",
	CheckNodesReady:      "Nodes Ready",
	CheckSystemWorkloads: "System workloads",
	CheckScheduling:      "Scheduling capacity",
	CheckEtcd:            "etcd quorum",
	CheckEtcdStore:       "etcd backend store",
	CheckAddons:          "Add-on availability",
	CheckCerts:           "Control-plane certificates",
	CheckBackup:          "Control-plane backup",
	CheckRepair:          "Automatic repair",
}

// etcdQuotaWarn is how much of etcd's backend quota may be occupied before the store check flips
// from healthy to degraded. Well below 1.0 deliberately: at 1.0 etcd arms NOSPACE and the cluster is
// already read-only, so a check that only complains there is a post-mortem, not a warning.
const etcdQuotaWarn = 0.75

// EtcdStoreCheck reports the health of the cluster's etcd backend store from the status the
// reconciler observed and stamped on the cluster (domain.Cluster.Etcd). Like CertCheck it is derived
// from stored observed state rather than a live probe, so the fake and the real checker report it
// identically - and it covers the gap the existing etcd-quorum check cannot: /livez/etcd stays
// HEALTHY on a cluster that has hit its quota and gone read-only, because the members are all up and
// in quorum. They just refuse every write.
func EtcdStoreCheck(c *domain.Cluster) domain.HealthCheck {
	s := c.Etcd
	if s == nil {
		return Check(CheckEtcdStore, domain.HealthUnknown, "etcd backend store not yet observed")
	}
	size := domain.HumanBytes(s.DBBytes)
	switch {
	case s.HasAlarm(domain.EtcdAlarmCorrupt):
		// Not something defragmentation fixes, and not something the platform will try to: a CORRUPT
		// alarm means the members disagree about data and needs a human with a backup.
		return Check(CheckEtcdStore, domain.HealthUnhealthy, "etcd CORRUPT alarm armed - members disagree about data")
	case s.HasAlarm(domain.EtcdAlarmNoSpace):
		return Check(CheckEtcdStore, domain.HealthUnhealthy,
			fmt.Sprintf("etcd NOSPACE alarm armed - cluster is READ-ONLY (%s of %s quota)", size, domain.HumanBytes(s.EffectiveQuotaBytes())))
	case s.QuotaUsage() >= etcdQuotaWarn:
		return Check(CheckEtcdStore, domain.HealthDegraded,
			fmt.Sprintf("etcd backend store is %s - %.0f%% of its %s quota", size, s.QuotaUsage()*100, domain.HumanBytes(s.EffectiveQuotaBytes())))
	default:
		// An UNTUNED member (no --quota-backend-bytes, so etcd's stock 2GiB) is reported, not
		// penalized. It is deliberately not its own degraded case: every cluster created before the
		// controlplane_etcd role is untuned, the role only applies at kubeadm init/join, and the one
		// thing that converges it - the defrag pass's tune.yml - will never fire on a cluster whose
		// store is far below the fragmentation floor. A degraded state nothing converges is a
		// permanently red panel, which trains people to ignore the panel. The danger itself is
		// already covered: EffectiveQuotaBytes falls back to 2GiB, so the quota case above fires at
		// 75% of the REAL ceiling whether or not anyone raised it.
		note := ""
		if s.QuotaBytes == 0 {
			note = fmt.Sprintf(" (on etcd's default %s quota)", domain.HumanBytes(domain.EtcdDefaultQuotaBytes))
		}
		return Check(CheckEtcdStore, domain.HealthHealthy,
			fmt.Sprintf("etcd backend store is %s, %.0f%% fragmented%s", size, s.FragmentationRatio()*100, note))
	}
}

// certWarnWindow is how close to expiry the certificate health check flips from healthy to degraded.
// Independent of the reconciler's renewal window (KAAS_CERT_RENEW_WINDOW) - this is the panel's
// warning threshold, not the trigger - but a comparable ~30 days so "degraded" and "renewal pending"
// coincide in practice.
const certWarnWindow = 30 * 24 * time.Hour

// CertCheck reports control-plane certificate health from the expiry observed by the reconciler and
// stamped on the cluster (domain.Cluster.CertNotAfter). It is derived from stored observed state, not
// a live probe, so the fake and the real checker report it identically. Unknown when expiry hasn't
// been observed yet - a brand-new cluster before its first Ready tick, or automatic rotation disabled.
func CertCheck(c *domain.Cluster) domain.HealthCheck {
	if c.CertNotAfter == nil {
		return Check(CheckCerts, domain.HealthUnknown, "certificate expiry not yet observed")
	}
	days := int(time.Until(*c.CertNotAfter).Hours() / 24)
	switch {
	case !c.CertNotAfter.After(time.Now()):
		return Check(CheckCerts, domain.HealthUnhealthy, "control-plane certificates expired")
	case time.Until(*c.CertNotAfter) <= certWarnWindow:
		return Check(CheckCerts, domain.HealthDegraded, fmt.Sprintf("control-plane certificates expire in %d day(s) - renewal pending", days))
	default:
		return Check(CheckCerts, domain.HealthHealthy, fmt.Sprintf("control-plane certificates valid for %d day(s)", days))
	}
}

// BackupCheck reports whether the cluster's control-plane backups are actually happening, derived -
// like CertCheck and EtcdStoreCheck - from stored observed state rather than a live probe, so the
// fake and the real checker report it identically.
//
// It exists because THE CLASSIC WAY A BACKUP FAILS IS SILENTLY. A snapshot that errors every time
// leaves no trace on a healthy-looking cluster: the phase settles back to Ready, the timeline scrolls
// past, and the fact that there is nothing to restore from is discovered during the recovery. The
// staleness of the newest snapshot is the one signal that makes that visible before it matters.
//
// Degraded rather than unhealthy when stale: the cluster is serving perfectly well. What is broken
// is its recoverability, which is a different and slower emergency.
func BackupCheck(c *domain.Cluster, p domain.EtcdSnapshotPolicy) domain.HealthCheck {
	if !p.Enabled {
		return Check(CheckBackup, domain.HealthUnknown, "automatic control-plane backups are disabled")
	}
	if c.EtcdSnapshotAt == nil {
		// Not yet degraded: every cluster is in this state between being created and its first
		// snapshot, and a check that goes red on every new cluster is a check people learn to ignore.
		return Check(CheckBackup, domain.HealthUnknown, "no control-plane backup taken yet")
	}
	age := time.Since(*c.EtcdSnapshotAt)
	// Twice the interval, so a single missed or slow run is tolerated and a genuinely stuck one is
	// not - the same reasoning behind not alerting on one failed scrape.
	if p.Interval > 0 && age > 2*p.Interval {
		return Check(CheckBackup, domain.HealthDegraded, fmt.Sprintf(
			"newest control-plane backup is %s old, past the %s cadence - this cluster may not be recoverable",
			domain.HumanDuration(age), domain.HumanDuration(p.Interval)))
	}
	return Check(CheckBackup, domain.HealthHealthy,
		fmt.Sprintf("control-plane backup taken %s ago", domain.HumanDuration(age)))
}

// RepairCheck reports what automatic repair currently believes about the cluster, from the repair
// state the reconciler stamped (domain.ClusterRepair). Derived state again, so both checkers agree.
//
// The case worth having this for is SUSPENDED: a node the platform has tried and given up on. That
// is the state where self-healing has stopped and a human is needed, and without a check for it the
// only evidence is an event that scrolled past hours ago. Everything else here is informational -
// an in-flight repair is the platform working, not a problem.
func RepairCheck(c *domain.Cluster, p domain.RepairPolicy) domain.HealthCheck {
	if !p.Enabled {
		return Check(CheckRepair, domain.HealthUnknown, "automatic repair is disabled")
	}
	r := c.Repair
	if r == nil || r.ObservedAt.IsZero() {
		return Check(CheckRepair, domain.HealthUnknown, "repair state not yet observed")
	}
	if suspended := r.SuspendedNodes(); len(suspended) > 0 {
		return Check(CheckRepair, domain.HealthUnhealthy, fmt.Sprintf(
			"repair suspended on %s - automatic recovery has stopped and needs a human",
			strings.Join(suspended, ", ")))
	}
	if r.InFlight() {
		return Check(CheckRepair, domain.HealthDegraded,
			fmt.Sprintf("repairing %s (%s)", r.Target, r.Action))
	}
	if n := r.Unhealthy(); n > 0 {
		return Check(CheckRepair, domain.HealthDegraded, fmt.Sprintf("%d node(s) faulty - repair pending", n))
	}
	return Check(CheckRepair, domain.HealthHealthy, "no faults; automatic repair idle")
}

// Check builds a HealthCheck with the canonical name for id.
func Check(id string, status domain.HealthStatus, summary string) domain.HealthCheck {
	return domain.HealthCheck{ID: id, Name: checkNames[id], Status: status, Summary: summary}
}

// Result is what a Checker returns for one cluster: the evaluated checks and per-node detail. The
// reconciler stamps the cluster id / collection time and computes the rollup (domain.RollupHealth),
// so a Checker only produces the raw findings.
type Result struct {
	Checks []domain.HealthCheck
	Nodes  []domain.NodeHealth
}

// Checker evaluates a cluster's health. Implementations must be safe to call repeatedly on the
// reconciler's health ticker; a transient failure is logged and retried next tick, never fatal.
// A checker reports an unreachable API server as an unhealthy *check*, not an error - an error is
// reserved for a failure to evaluate at all (e.g. the kubeconfig couldn't be prepared).
type Checker interface {
	// Check evaluates the cluster given its admin kubeconfig and returns the findings.
	Check(ctx context.Context, c *domain.Cluster, kubeconfig []byte) (Result, error)
}

// Fake synthesizes an all-healthy snapshot derived from control-plane state (node phases, control
// plane count, installed add-ons), so the portal's health panel is populated without a real
// cluster. It reports genuine detail where the store has it - it counts NotReady nodes from
// c.Nodes - but has no way to observe real workload, scheduling, or cordon state (domain.Node
// carries no such field), so those checks are healthy by construction (the real checker is where
// genuine degradation, including a cordoned node, shows).
type Fake struct{}

func NewFake() *Fake { return &Fake{} }

func (Fake) Check(_ context.Context, c *domain.Cluster, _ []byte) (Result, error) {
	nodes := make([]domain.NodeHealth, 0, len(c.Nodes))
	ready := 0
	for _, n := range c.Nodes {
		ok := n.Phase == "" || n.Phase == "ready"
		if ok {
			ready++
		}
		nodes = append(nodes, domain.NodeHealth{NodeName: n.VMName, Ready: ok})
	}
	total := len(c.Nodes)

	nodesStatus, nodesSummary := domain.HealthHealthy, fmt.Sprintf("%d/%d nodes Ready", ready, total)
	switch {
	case total == 0:
		nodesStatus, nodesSummary = domain.HealthUnknown, "no nodes provisioned"
	case ready < total:
		nodesStatus = domain.HealthUnhealthy
	}

	cp := c.ControlPlaneCount()
	checks := []domain.HealthCheck{
		Check(CheckAPIServer, domain.HealthHealthy, "control plane responding"),
		Check(CheckNodesReady, nodesStatus, nodesSummary),
		Check(CheckSystemWorkloads, domain.HealthHealthy, "core kube-system pods running"),
		Check(CheckScheduling, domain.HealthHealthy, "no unschedulable pods; no node pressure"),
		Check(CheckEtcd, domain.HealthHealthy, fmt.Sprintf("etcd healthy (%d-member quorum)", cp)),
		EtcdStoreCheck(c),
		fakeAddonCheck(c),
		CertCheck(c),
	}
	return Result{Checks: checks, Nodes: nodes}, nil
}

// fakeAddonCheck reports installed-add-on availability, unknown when the cluster runs none (so an
// add-on-free cluster doesn't get a meaningless green check).
func fakeAddonCheck(c *domain.Cluster) domain.HealthCheck {
	installed := 0
	for _, a := range c.Addons {
		if a.Phase == "installed" {
			installed++
		}
	}
	if installed == 0 {
		return Check(CheckAddons, domain.HealthUnknown, "no add-ons installed")
	}
	return Check(CheckAddons, domain.HealthHealthy, fmt.Sprintf("%d/%d add-ons available", installed, installed))
}
