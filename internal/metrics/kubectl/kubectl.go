// Package kubectl is the real metrics.Collector: it reads live node usage from a cluster's
// in-cluster metrics API with `kubectl get --raw`, using the cluster's admin kubeconfig.
//
// Usage comes from metrics.k8s.io (served by the metrics-server add-on); node capacity comes
// from the core API. Both are quantities in Kubernetes' notation (CPU like "250m" or "2",
// memory like "3925132Ki"), parsed here into millicores and bytes. Read-only and idempotent:
// the reconciler calls it on a slow ticker and a transient failure is simply retried.
package kubectl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
)

type Config struct {
	Bin          string       // "kubectl"
	WorkDir      string       // per-cluster dir for the temp kubeconfig
	KubeProxyURL string       // SOCKS proxy to reach the API server through; "" (local KVM) = direct
	Log          *slog.Logger // required
}

type Collector struct{ cfg Config }

func New(cfg Config) (*Collector, error) {
	if cfg.Bin == "" {
		cfg.Bin = "kubectl"
	}
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("kubectl: WorkDir is required")
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("kubectl: Log is required")
	}
	return &Collector{cfg: cfg}, nil
}

func (m *Collector) CollectNodes(ctx context.Context, c *domain.Cluster, kubeconfig []byte) ([]domain.NodeMetrics, error) {
	kcPath, err := m.writeKubeconfig(c.ID, kubeconfig)
	if err != nil {
		return nil, err
	}
	swallow := func(string) {} // stderr is only interesting when the command actually fails

	capOut, err := m.raw(ctx, kcPath, swallow, "/api/v1/nodes")
	if err != nil {
		return nil, fmt.Errorf("kubectl: read node capacity: %w", err)
	}
	usageOut, err := m.raw(ctx, kcPath, swallow, "/apis/metrics.k8s.io/v1beta1/nodes")
	if err != nil {
		return nil, fmt.Errorf("kubectl: read node metrics (is metrics-server ready?): %w", err)
	}

	capacity, err := parseCapacity(capOut)
	if err != nil {
		return nil, err
	}
	usage, err := parseUsage(usageOut)
	if err != nil {
		return nil, err
	}

	out := make([]domain.NodeMetrics, 0, len(usage))
	for name, u := range usage {
		cp := capacity[name]
		out = append(out, domain.NodeMetrics{
			NodeName:         name,
			CPUUsedMilli:     u.cpuMilli,
			CPUCapacityMilli: cp.cpuMilli,
			MemUsedBytes:     u.memBytes,
			MemCapacityBytes: cp.memBytes,
		})
	}
	return out, nil
}

func (m *Collector) raw(ctx context.Context, kcPath string, emit func(string), path string) ([]byte, error) {
	return procstream.Capture(ctx, "", os.Environ(), emit, m.cfg.Bin,
		"--kubeconfig", kcPath, "get", "--raw", path)
}

type res struct {
	cpuMilli int64
	memBytes int64
}

// parseCapacity pulls per-node allocatable CPU/memory from a core `/api/v1/nodes` list.
func parseCapacity(b []byte) (map[string]res, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Allocatable struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("kubectl: decode nodes: %w", err)
	}
	out := make(map[string]res, len(list.Items))
	for _, it := range list.Items {
		out[it.Metadata.Name] = res{
			cpuMilli: parseCPU(it.Status.Allocatable.CPU),
			memBytes: parseMem(it.Status.Allocatable.Memory),
		}
	}
	return out, nil
}

// parseUsage pulls per-node CPU/memory usage from a metrics.k8s.io node list.
func parseUsage(b []byte) (map[string]res, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("kubectl: decode metrics: %w", err)
	}
	out := make(map[string]res, len(list.Items))
	for _, it := range list.Items {
		out[it.Metadata.Name] = res{
			cpuMilli: parseCPU(it.Usage.CPU),
			memBytes: parseMem(it.Usage.Memory),
		}
	}
	return out, nil
}

// parseCPU converts a Kubernetes CPU quantity to millicores: "250m" → 250, "2" → 2000,
// "1500000n" (nanocores, as metrics-server reports) → 1500. Best-effort: 0 on a bad value.
func parseCPU(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	switch {
	case strings.HasSuffix(q, "n"):
		return atoi(strings.TrimSuffix(q, "n")) / 1_000_000
	case strings.HasSuffix(q, "u"):
		return atoi(strings.TrimSuffix(q, "u")) / 1_000
	case strings.HasSuffix(q, "m"):
		return atoi(strings.TrimSuffix(q, "m"))
	default:
		return atoi(q) * 1000
	}
}

// memSuffixes maps Kubernetes binary/decimal memory suffixes to their byte multiplier.
var memSuffixes = []struct {
	suffix string
	mult   int64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// parseMem converts a Kubernetes memory quantity to bytes: "3925132Ki" → bytes, "512Mi", "2G".
// Best-effort: 0 on a bad value.
func parseMem(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	for _, s := range memSuffixes {
		if strings.HasSuffix(q, s.suffix) {
			return atoi(strings.TrimSuffix(q, s.suffix)) * s.mult
		}
	}
	return atoi(q) // plain bytes
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func (m *Collector) writeKubeconfig(clusterID string, kc []byte) (string, error) {
	dir := filepath.Join(m.cfg.WorkDir, clusterID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Routes the metrics read through the KVM host when the cluster isn't locally reachable; no-op
	// when it is (see internal/kubeconfig).
	kc, err := kubeconfig.WithProxy(kc, m.cfg.KubeProxyURL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
