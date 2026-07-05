// Package kube is the request-driven cluster-query seam behind the portal's Workloads and Storage
// pages: it lists and inspects the live workloads running inside a Ready cluster (Deployments,
// StatefulSets, DaemonSets, Jobs, CronJobs), their pods, YAML, events and logs, and scales them;
// and it reads the cluster's storage objects (PersistentVolumeClaims and StorageClasses, read-only
// - see storage.go).
//
// Unlike the metrics/health seams - which the reconciler samples on a slow ticker and snapshots
// into the store - this seam is on-demand and interactive: the API calls it per request to browse
// and act on workloads. As with every seam that touches a cluster, only the worker can reach the
// cluster API server (see docs/networking.md), so the real implementation (internal/kube/kubectl)
// runs there and the API reaches it through the worker exec agent (internal/kube/proxy →
// internal/shell/agent). The Fake synthesizes plausible workloads from control-plane state so the
// whole page stays demoable under `make up-fake`, mirroring the fake shell.
//
// Fidelity/security shortcut (shared with the shell, see internal/shell): the exec channel forwards
// a cluster kubeconfig to a host-networked worker over a plaintext localhost bearer token. Read vs.
// write is enforced two ways - the API refuses a scale from a read-role group member (403), and the
// kubeconfig it forwards for a read-role member is the RBAC-limited viewer credential. Production
// would front this with authn/RBAC, an RBAC-scoped kubeconfig, audit, and TLS on the API↔worker hop.
package kube

import (
	"context"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// WorkloadKind is one of the five controller kinds the Workloads page lists. The lowercase value is
// the wire form the portal uses in URLs and JSON.
type WorkloadKind string

const (
	KindDeployment  WorkloadKind = "deployment"
	KindStatefulSet WorkloadKind = "statefulset"
	KindDaemonSet   WorkloadKind = "daemonset"
	KindJob         WorkloadKind = "job"
	KindCronJob     WorkloadKind = "cronjob"
)

// AllKinds is the canonical order the page groups and filters by.
var AllKinds = []WorkloadKind{KindDeployment, KindStatefulSet, KindDaemonSet, KindJob, KindCronJob}

// Resource returns the plural kubectl resource name for the kind (e.g. "deployments").
func (k WorkloadKind) Resource() string {
	switch k {
	case KindDeployment:
		return "deployments"
	case KindStatefulSet:
		return "statefulsets"
	case KindDaemonSet:
		return "daemonsets"
	case KindJob:
		return "jobs"
	case KindCronJob:
		return "cronjobs"
	default:
		return string(k)
	}
}

// Scalable reports whether a kind supports replica scaling. Deployments and StatefulSets do;
// DaemonSets are node-driven, Jobs fixed at creation, CronJobs suspend/resume instead.
func (k WorkloadKind) Scalable() bool { return k == KindDeployment || k == KindStatefulSet }

// ParseKind normalizes a URL/CLI form (plural, singular, or Kind) to a WorkloadKind.
func ParseKind(s string) (WorkloadKind, bool) {
	switch strings.ToLower(strings.TrimSuffix(s, "s")) {
	case "deployment", "deploy":
		return KindDeployment, true
	case "statefulset", "sts":
		return KindStatefulSet, true
	case "daemonset", "ds":
		return KindDaemonSet, true
	case "job":
		return KindJob, true
	case "cronjob", "cj":
		return KindCronJob, true
	default:
		return "", false
	}
}

// WorkloadRef identifies a single workload within a cluster.
type WorkloadRef struct {
	Kind      WorkloadKind `json:"kind"`
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
}

// WorkloadSummary is one row on the Workloads list. ReadyReplicas/DesiredReplicas are normalized
// across kinds (DaemonSet ready/desired scheduled; Job succeeded/completions), so the UI can render
// a single "ready/desired" column. Status is a short kubectl-style rollup ("Running", "Complete",
// "Suspended", …). Age is derived by the UI from CreatedAt.
type WorkloadSummary struct {
	Kind            WorkloadKind `json:"kind"`
	Namespace       string       `json:"namespace"`
	Name            string       `json:"name"`
	ReadyReplicas   int          `json:"ready_replicas"`
	DesiredReplicas int          `json:"desired_replicas"`
	Status          string       `json:"status"`
	Images          []string     `json:"images"`
	CreatedAt       time.Time    `json:"created_at"`
	Schedule        string       `json:"schedule,omitempty"`  // CronJob only
	Suspended       bool         `json:"suspended,omitempty"` // CronJob only
}

// WorkloadDetail is a single workload's full view: the summary plus rollout detail, selector,
// labels, containers, conditions, and the pods it currently owns.
type WorkloadDetail struct {
	WorkloadSummary
	UpdatedReplicas   int               `json:"updated_replicas"`
	AvailableReplicas int               `json:"available_replicas"`
	Strategy          string            `json:"strategy,omitempty"`
	Selector          map[string]string `json:"selector,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Containers        []Container       `json:"containers"`
	Conditions        []Condition       `json:"conditions"`
	Pods              []PodInfo         `json:"pods"`
}

// Container is one container spec entry (name + image), for the Overview and the logs selector.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// Condition is a workload status condition (Available, Progressing, Complete, …).
type Condition struct {
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
}

// PodInfo is one pod owned (directly or transitively) by a workload. Ready is the kubectl "n/m"
// container-ready form; Containers lists container names so the logs viewer can offer a selector.
type PodInfo struct {
	Name       string    `json:"name"`
	Ready      string    `json:"ready"`
	Status     string    `json:"status"`
	Restarts   int       `json:"restarts"`
	Node       string    `json:"node"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	Containers []string  `json:"containers"`
}

// Event is a Kubernetes event scoped to a workload or one of its pods.
type Event struct {
	Type     string    `json:"type"` // Normal | Warning
	Reason   string    `json:"reason"`
	Message  string    `json:"message"`
	Count    int       `json:"count"`
	LastSeen time.Time `json:"last_seen"`
	Object   string    `json:"object"` // involvedObject "Kind/name"
}

// LogRef identifies a pod (and optional container) log stream to tail.
type LogRef struct {
	Namespace string
	Pod       string
	Container string
	TailLines int
	Follow    bool
}

// LogSink receives log output as it arrives (raw bytes, as read from kubectl). Implementations
// write to the browser WebSocket (API side) or the proxied worker stream. Safe for one writer.
type LogSink interface {
	Write(p []byte) error
}

// Client reads and mutates the live workloads in a Ready cluster given its (admin or viewer)
// kubeconfig. Every method must be safe to call per request; a transient failure is a normal error
// the API surfaces, never fatal. namespace == "" means "all namespaces" for Workloads/Namespaces.
//
// It also embeds StorageReader (see storage.go), the read-only PersistentVolumeClaim/StorageClass
// surface behind the portal's Storage page, and NetworkReader (see network.go), the read-only
// Service/Gateway/Route surface behind the Networking page: those are core Kubernetes objects (and,
// for the Gateway API, ordinary CRDs) read the same way over the same transport, so they share this
// seam rather than getting one of their own.
type Client interface {
	StorageReader
	NetworkReader
	ConfigReader

	Namespaces(ctx context.Context, c *domain.Cluster, kubeconfig []byte) ([]string, error)
	Workloads(ctx context.Context, c *domain.Cluster, kubeconfig []byte, namespace string) ([]WorkloadSummary, error)
	Workload(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref WorkloadRef) (*WorkloadDetail, error)
	Manifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref WorkloadRef) (string, error)
	Events(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref WorkloadRef) ([]Event, error)
	// Scale sets a workload's replica count (Deployments/StatefulSets). readOnly rejects the write -
	// the real backend also has RBAC enforce it, but the fake has no API server so it honors the flag.
	Scale(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref WorkloadRef, replicas int, readOnly bool) error
	// Logs streams a pod/container's logs to sink, blocking until the stream ends, ctx is cancelled,
	// or (when ref.Follow is false) kubectl exits after emitting the tail.
	Logs(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref LogRef, sink LogSink) error

	// MintUserKubeconfig issues a per-user, client-certificate kubeconfig for username at the given
	// role, signed by the cluster CA - the credential a tenant downloads to reach their cluster with
	// their OWN identity (CN=username, O=kaas:writers|kaas:readers) rather than a shared admin/viewer
	// credential. The passed kubeconfig is the platform's ADMIN config: it is the authority that mints
	// the cert (create+approve a CertificateSigningRequest) and the source of the API server + CA the
	// result copies - it is never returned. ttl bounds the cert's validity; the returned time is its
	// actual NotAfter (the signer may cap it). Request-driven and self-contained: the real backend runs
	// the CSR dance over the same worker exec agent as the rest of this seam, so no CA key ever leaves
	// the control plane and the user's private key never leaves this process. See internal/domain
	// KubeGroupForRole and the viewer_kubeconfig role for the matching in-cluster RBAC.
	MintUserKubeconfig(ctx context.Context, c *domain.Cluster, adminKubeconfig []byte, username string, role domain.GroupRole, ttl time.Duration) (kubeconfig []byte, notAfter time.Time, err error)
}
