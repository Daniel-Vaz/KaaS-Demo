// Package metrics is the resource-usage telemetry seam: read live per-node CPU/memory
// consumption from a Ready cluster's in-cluster metrics API (metrics-server, metrics.k8s.io).
//
// Unlike the other seams this one is read-only and its output is live/observed telemetry, not
// desired state: the reconciler samples it on a slow ticker and upserts the latest snapshot into
// the store, and the API serves that snapshot read-through (only the worker can reach the cluster
// network - see docs/networking.md). Fake synthesizes plausible, drifting usage so the portal's
// gauges move without a real cluster; the real implementation shells out to `kubectl top`.
package metrics

import (
	"context"
	"hash/fnv"
	"math"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// AddonName is the catalog add-on that provides these metrics. Collection is gated on it being
// installed, and the portal hides the usage panels when it is absent.
const AddonName = "metrics-server"

// Collector reads live per-node resource usage for a cluster. Implementations must be safe to
// call repeatedly on the reconciler's metrics ticker; a transient failure is logged and retried
// next tick, never fatal.
type Collector interface {
	// CollectNodes returns a usage sample per node, given the cluster and its admin kubeconfig.
	CollectNodes(ctx context.Context, c *domain.Cluster, kubeconfig []byte) ([]domain.NodeMetrics, error)
}

// Fake synthesizes plausible node usage from each node's t-shirt size (the capacity) and a
// slowly drifting, per-node pseudo-random load, so the UI shows realistic, moving gauges without
// a real metrics-server. Deterministic in the node name (stable baseline) plus a time term (drift).
type Fake struct{}

func NewFake() *Fake { return &Fake{} }

func (Fake) CollectNodes(_ context.Context, c *domain.Cluster, _ []byte) ([]domain.NodeMetrics, error) {
	now := float64(time.Now().Unix())
	out := make([]domain.NodeMetrics, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		// Capacity is per NODE, not per cluster: a worker's shape comes from its own pool, so a
		// cluster's nodes can differ. Hoisting one Sizes[c.Size] lookup out of this loop (as this did
		// before node pools) would report the control plane's capacity for every worker.
		spec := c.NodeSize(n)
		cpuCap := int64(spec.CPUs) * 1000         // cores → millicores
		memCap := int64(spec.MemMB) * 1024 * 1024 // MB → bytes
		// Control planes idle a little higher than workers; each node gets its own phase/baseline
		// from its name so the fleet doesn't move in lockstep.
		base := 0.25
		if n.Role == domain.RoleControlPlane {
			base = 0.35
		}
		cpuLoad := load(base, 0.18, now/40, phase(n.VMName+"/cpu"))
		memLoad := load(base+0.15, 0.10, now/90, phase(n.VMName+"/mem"))
		out = append(out, domain.NodeMetrics{
			NodeName:         n.VMName,
			CPUUsedMilli:     int64(cpuLoad * float64(cpuCap)),
			CPUCapacityMilli: cpuCap,
			MemUsedBytes:     int64(memLoad * float64(memCap)),
			MemCapacityBytes: memCap,
		})
	}
	return out, nil
}

// load returns a fraction in [0.02, 0.97]: a baseline plus a sine drift, so gauges breathe.
func load(base, amp, t, ph float64) float64 {
	v := base + amp*math.Sin(t+ph)
	return math.Max(0.02, math.Min(0.97, v))
}

// phase derives a stable per-series phase offset in [0, 2π) from a key.
func phase(key string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum32()%360) * math.Pi / 180
}
