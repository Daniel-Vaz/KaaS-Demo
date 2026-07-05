// Package security is the request-driven cluster security-posture seam behind the portal's Security
// page: it reads the Trivy Operator's report CRDs from a Ready cluster and returns typed summaries
// the portal renders as severity rollups, risk tables, and per-report finding lists.
//
// Trivy Operator (installed by the trivy-operator add-on) continuously scans a cluster's workloads
// and writes its findings back as Kubernetes custom resources - VulnerabilityReports (image CVEs),
// ConfigAuditReports (workload misconfigurations), ExposedSecretReports (secrets baked into images),
// and RbacAssessmentReports (over-permissive RBAC). This seam simply reads those CRs; the scanning
// itself is done by the operator inside the cluster, not here.
//
// Like the monitoring/kube seams it is on-demand and interactive (queried per request, never sampled
// on the reconcile ticker), and like every seam that touches a cluster only the worker can reach the
// cluster API server (see docs/networking.md): the real implementation (internal/security/kubectl)
// reuses the same worker exec agent the Workloads/Monitoring seams use - each query is a
// `kubectl get <report>.aquasecurity.github.io -A -o json` - so no new API↔worker transport is added.
// The Fake synthesizes a plausible, drifting set of reports from control-plane state so the whole
// page is demoable under `make up-fake`.
//
// Fidelity/security shortcut (shared with monitoring): the queries run with the cluster ADMIN
// kubeconfig server-side, because the built-in `view` role a read-role member holds does not cover
// Trivy's custom resources. The data returned is read-only security-posture metadata - Trivy already
// redacts the actual matched secret values in ExposedSecretReports - and access is still gated by the
// app's owner/group/admin view check. Production would mint an RBAC-scoped read token for the Trivy
// CRD group instead of widening viewer RBAC.
package security

import (
	"context"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// AddonName is the catalog add-on that provides these reports. The Security page is gated on it being
// installed; a cluster without it exposes no security data.
const AddonName = "trivy-operator"

// APIGroup is the Trivy Operator CRD group. Report resources are fetched fully-qualified
// (e.g. "vulnerabilityreports.aquasecurity.github.io") so a name collision with another CRD can't
// resolve the wrong resource.
const APIGroup = "aquasecurity.github.io"

// Kind is one of the four Trivy report families the Security page surfaces. The lowercase value is
// the wire form the portal uses in URLs and JSON.
type Kind string

const (
	KindVulnerability  Kind = "vulnerability"  // VulnerabilityReport - image CVEs
	KindConfigAudit    Kind = "configaudit"    // ConfigAuditReport - workload misconfigurations
	KindExposedSecret  Kind = "exposedsecret"  // ExposedSecretReport - secrets baked into images
	KindRbacAssessment Kind = "rbacassessment" // RbacAssessmentReport - over-permissive RBAC
)

// AllKinds is the canonical order the page tabs and aggregates by.
var AllKinds = []Kind{KindVulnerability, KindConfigAudit, KindExposedSecret, KindRbacAssessment}

// KindMeta describes one report family for the page's tab bar and rendering. Resource is the Trivy
// CRD plural (unqualified) this kind reads; the querier qualifies it with APIGroup.
type KindMeta struct {
	ID          Kind   `json:"id"`
	Title       string `json:"title"`
	Resource    string `json:"-"`            // trivy CRD plural, e.g. "vulnerabilityreports"
	FindingNoun string `json:"finding_noun"` // singular label for one finding ("vulnerability", "check", …)
	// HasArtifact marks the kinds whose reports are about a container image (vulnerability, exposed
	// secret), so the portal shows an image column; the others are about a workload/RBAC object.
	HasArtifact bool `json:"has_artifact"`
	// Description is a one-line explanation the page shows under the tab.
	Description string `json:"description"`
}

// kindMetas is the single registry both the Fake and the real querier read, so they always agree on
// which kinds exist and how each is labelled.
var kindMetas = map[Kind]KindMeta{
	KindVulnerability: {
		ID: KindVulnerability, Title: "Vulnerabilities", Resource: "vulnerabilityreports",
		FindingNoun: "vulnerability", HasArtifact: true,
		Description: "Known CVEs found in the container images running in this cluster.",
	},
	KindConfigAudit: {
		ID: KindConfigAudit, Title: "Misconfigurations", Resource: "configauditreports",
		FindingNoun: "check", HasArtifact: false,
		Description: "Workload configuration checks against security best practices.",
	},
	KindExposedSecret: {
		ID: KindExposedSecret, Title: "Exposed Secrets", Resource: "exposedsecretreports",
		FindingNoun: "secret", HasArtifact: true,
		Description: "Sensitive credentials accidentally baked into container images.",
	},
	KindRbacAssessment: {
		ID: KindRbacAssessment, Title: "RBAC Assessment", Resource: "rbacassessmentreports",
		FindingNoun: "check", HasArtifact: false,
		Description: "Roles granting more access than they should - over-permissive RBAC.",
	},
}

// Meta returns the descriptor for a kind.
func Meta(k Kind) (KindMeta, bool) {
	m, ok := kindMetas[k]
	return m, ok
}

// KindMetas returns the kind descriptors in canonical order (for the page's tab bar).
func KindMetas() []KindMeta {
	out := make([]KindMeta, 0, len(AllKinds))
	for _, k := range AllKinds {
		out = append(out, kindMetas[k])
	}
	return out
}

// ParseKind normalizes a URL form to a Kind.
func ParseKind(s string) (Kind, bool) {
	k := Kind(s)
	if _, ok := kindMetas[k]; ok {
		return k, true
	}
	return "", false
}

// Severity is a normalized Trivy severity (lowercased). Trivy emits CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN.
type Severity string

const (
	SevCritical Severity = "critical"
	SevHigh     Severity = "high"
	SevMedium   Severity = "medium"
	SevLow      Severity = "low"
	SevUnknown  Severity = "unknown"
)

// Severities is severity order, most-severe first - the order the page renders bars and legends in.
var Severities = []Severity{SevCritical, SevHigh, SevMedium, SevLow, SevUnknown}

// Counts is a severity breakdown, the common currency of every summary in this package.
type Counts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
}

// Add increments the bucket for a severity (unknown/unrecognised → Unknown).
func (c *Counts) Add(s Severity) { c.AddN(s, 1) }

// AddN adds n to the bucket for a severity.
func (c *Counts) AddN(s Severity, n int) {
	switch s {
	case SevCritical:
		c.Critical += n
	case SevHigh:
		c.High += n
	case SevMedium:
		c.Medium += n
	case SevLow:
		c.Low += n
	default:
		c.Unknown += n
	}
}

// Plus returns the element-wise sum, for rolling several reports into one aggregate.
func (c Counts) Plus(o Counts) Counts {
	return Counts{c.Critical + o.Critical, c.High + o.High, c.Medium + o.Medium, c.Low + o.Low, c.Unknown + o.Unknown}
}

// Total is the sum across every severity.
func (c Counts) Total() int { return c.Critical + c.High + c.Medium + c.Low + c.Unknown }

// Resource is the Kubernetes object a report is about - a workload for vulnerability/configaudit/
// exposedsecret, a Role/ClusterRole for rbacassessment.
type Resource struct {
	Kind      string `json:"kind"`                // Deployment, ReplicaSet, Role, …
	Name      string `json:"name"`                // owning workload/object name
	Container string `json:"container,omitempty"` // vulnerability/exposedsecret are per-container
}

// Artifact is the container image a vulnerability/exposed-secret report scanned.
type Artifact struct {
	Registry   string `json:"registry,omitempty"`
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

// Image renders the artifact as a human "repo:tag" (or "@digest" when untagged).
func (a *Artifact) Image() string {
	if a == nil || a.Repository == "" {
		return ""
	}
	name := a.Repository
	if a.Registry != "" && a.Registry != "index.docker.io" && a.Registry != "docker.io" {
		name = a.Registry + "/" + a.Repository
	}
	if a.Tag != "" {
		return name + ":" + a.Tag
	}
	if a.Digest != "" {
		return name + "@" + a.Digest
	}
	return name
}

// Report is one Trivy report CR summarized into a table row: what was scanned, and its severity
// rollup. The finding list is fetched separately (ReportDetail) since a single VulnerabilityReport
// can carry hundreds of CVEs.
type Report struct {
	Kind      Kind      `json:"kind"`
	Name      string    `json:"name"`      // the CR name
	Namespace string    `json:"namespace"` // the CR namespace
	Resource  Resource  `json:"resource"`
	Artifact  *Artifact `json:"artifact,omitempty"` // vulnerability/exposedsecret only
	Summary   Counts    `json:"summary"`
	Scanner   string    `json:"scanner,omitempty"` // e.g. "Trivy 0.58.0"
	UpdatedAt string    `json:"updated_at,omitempty"`
}

// Finding is one entry inside a report - a CVE, a failed config/RBAC check, or a matched secret.
// Only the fields relevant to the report's Kind are populated.
type Finding struct {
	ID       string   `json:"id"` // CVE-2024-1234 | KSV014 | ruleID
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`

	// vulnerability
	Resource         string  `json:"resource,omitempty"`          // vulnerable package
	InstalledVersion string  `json:"installed_version,omitempty"` // package version present
	FixedVersion     string  `json:"fixed_version,omitempty"`     // version that fixes it ("" = no fix)
	Score            float64 `json:"score,omitempty"`             // CVSS score
	Link             string  `json:"link,omitempty"`              // advisory URL

	// configaudit / rbacassessment
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
	Remediation string `json:"remediation,omitempty"`

	// exposedsecret
	Match  string `json:"match,omitempty"`  // redacted match (Trivy masks the secret itself)
	Target string `json:"target,omitempty"` // file the secret was found in
}

// ReportDetail is a report plus its full finding list, ordered most-severe first by the querier.
type ReportDetail struct {
	Report
	Findings []Finding `json:"findings"`
}

// KindStat is a report family's cluster-wide rollup for the Overview: how many report CRs exist and
// the summed severity counts across all of them.
type KindStat struct {
	Kind        Kind   `json:"kind"`
	Title       string `json:"title"`
	ReportCount int    `json:"report_count"`
	Totals      Counts `json:"totals"`
}

// ImageRisk is one image's aggregated vulnerability posture, for the Overview's "most vulnerable
// images" table.
type ImageRisk struct {
	Image     string   `json:"image"`
	Summary   Counts   `json:"summary"`
	Workloads []string `json:"workloads,omitempty"` // "namespace/name" workloads running the image
}

// NamespaceRisk is one namespace's total severity counts across every report kind, for the Overview's
// per-namespace heatmap.
type NamespaceRisk struct {
	Namespace string `json:"namespace"`
	Totals    Counts `json:"totals"`
}

// Overview is the cluster-wide security dashboard: per-kind rollups, the most vulnerable images, and a
// per-namespace risk breakdown. GeneratedAt is when the querier assembled it.
type Overview struct {
	GeneratedAt string          `json:"generated_at"`
	Kinds       []KindStat      `json:"kinds"`
	TopImages   []ImageRisk     `json:"top_images"`
	Namespaces  []NamespaceRisk `json:"namespaces"`
}

// Enabled reports whether the cluster has the Trivy Operator add-on installed. Mirrors
// monitoring.Enabled for kube-prometheus-stack.
func Enabled(c *domain.Cluster) bool {
	for _, a := range c.Addons {
		if a.Name == AddonName && a.Phase == "installed" {
			return true
		}
	}
	return false
}

// ErrUnknownKind is returned by a Querier for a kind not in the registry.
var ErrUnknownKind = errString("security: unknown report kind")

type errString string

func (e errString) Error() string { return string(e) }

// Querier reads Trivy's report CRDs from a Ready cluster given its admin kubeconfig. Every call is
// per-request; a transient failure is a normal error the API surfaces, never fatal.
type Querier interface {
	// Overview assembles the cluster-wide security dashboard (all four kinds rolled up).
	Overview(ctx context.Context, c *domain.Cluster, kubeconfig []byte) (*Overview, error)
	// Reports lists every report of one kind as a summary row (no finding lists).
	Reports(ctx context.Context, c *domain.Cluster, kubeconfig []byte, kind Kind) ([]Report, error)
	// Report returns one report CR with its full finding list.
	Report(ctx context.Context, c *domain.Cluster, kubeconfig []byte, kind Kind, namespace, name string) (*ReportDetail, error)
}
