// Package kubectl is the real security.Querier: it reads the Trivy Operator's report CRDs from a
// cluster with `kubectl get <report>.aquasecurity.github.io -A -o json`, run through the same Execer
// the Workloads/Monitoring seams use (a LocalExecer on the worker, or the API-side proxy that forwards
// to the worker exec agent - see internal/kube). No new API↔worker transport is added: a security
// query is just another `kubectl get`.
package kubectl

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/security"
)

// Execer runs a one-shot kubectl command for a cluster (structurally satisfied by the Workloads seam's
// LocalExecer and proxy Execer). Only Run is needed - the security seam never streams.
type Execer interface {
	Run(ctx context.Context, kubeconfig []byte, clusterID string, args []string) (kubectl.Result, error)
}

// Querier implements security.Querier on top of an Execer.
type Querier struct{ ex Execer }

// New returns a kubectl-backed security.Querier using ex to run kubectl.
func New(ex Execer) *Querier { return &Querier{ex: ex} }

// resourcesFor returns the Trivy CRD resource name(s) backing a kind. RBAC spans two CRDs: the
// namespaced rbacassessmentreports (Roles) and the cluster-scoped clusterrbacassessmentreports
// (ClusterRoles), so the page shows over-permissive ClusterRoles too.
func resourcesFor(kind security.Kind) []string {
	m, _ := security.Meta(kind)
	if kind == security.KindRbacAssessment {
		return []string{m.Resource, "clusterrbacassessmentreports"}
	}
	return []string{m.Resource}
}

// clusterScoped reports whether a Trivy resource is cluster-scoped (no namespace / no -A).
func clusterScoped(resource string) bool { return strings.HasPrefix(resource, "cluster") }

func (q *Querier) Reports(ctx context.Context, c *domain.Cluster, kc []byte, kind security.Kind) ([]security.Report, error) {
	if _, ok := security.Meta(kind); !ok {
		return nil, security.ErrUnknownKind
	}
	var out []security.Report
	for _, resource := range resourcesFor(kind) {
		items, err := q.list(ctx, c, kc, resource)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			out = append(out, it.report(kind))
		}
	}
	sortReports(out)
	return out, nil
}

func (q *Querier) Report(ctx context.Context, c *domain.Cluster, kc []byte, kind security.Kind, namespace, name string) (*security.ReportDetail, error) {
	if _, ok := security.Meta(kind); !ok {
		return nil, security.ErrUnknownKind
	}
	// Pick the CRD by scope: a namespaced report has a namespace, a ClusterRole assessment does not.
	resource := resourcesFor(kind)[0]
	if kind == security.KindRbacAssessment && namespace == "" {
		resource = "clusterrbacassessmentreports"
	}
	args := []string{"get", resource + "." + security.APIGroup, name, "-o", "json"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	res, err := q.ex.Run(ctx, kc, c.ID, args)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("kubectl get %s %s: %s", resource, name, firstLine(res.Stderr))
	}
	var it reportItem
	if err := json.Unmarshal(res.Stdout, &it); err != nil {
		return nil, fmt.Errorf("decode %s: %w", resource, err)
	}
	d := it.detail(kind)
	return &d, nil
}

func (q *Querier) Overview(ctx context.Context, c *domain.Cluster, kc []byte) (*security.Overview, error) {
	// Fetch each kind's report list concurrently, then aggregate.
	type res struct {
		kind  security.Kind
		items []reportItem
		err   error
	}
	results := make([]res, len(security.AllKinds))
	var wg sync.WaitGroup
	for i, kind := range security.AllKinds {
		wg.Add(1)
		go func(i int, kind security.Kind) {
			defer wg.Done()
			var all []reportItem
			for _, resource := range resourcesFor(kind) {
				items, err := q.list(ctx, c, kc, resource)
				if err != nil {
					results[i] = res{kind: kind, err: err}
					return
				}
				all = append(all, items...)
			}
			results[i] = res{kind: kind, items: all}
		}(i, kind)
	}
	wg.Wait()

	ov := &security.Overview{GeneratedAt: nowRFC3339()}
	nsTotals := map[string]security.Counts{}
	imageRisk := map[string]*security.ImageRisk{}
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		m, _ := security.Meta(r.kind)
		stat := security.KindStat{Kind: r.kind, Title: m.Title, ReportCount: len(r.items)}
		for _, it := range r.items {
			rep := it.report(r.kind)
			stat.Totals = stat.Totals.Plus(rep.Summary)
			nsTotals[rep.Namespace] = nsTotals[rep.Namespace].Plus(rep.Summary)
			if r.kind == security.KindVulnerability && rep.Artifact != nil {
				img := rep.Artifact.Image()
				ir := imageRisk[img]
				if ir == nil {
					ir = &security.ImageRisk{Image: img}
					imageRisk[img] = ir
				}
				ir.Summary = ir.Summary.Plus(rep.Summary)
				ir.Workloads = append(ir.Workloads, rep.Namespace+"/"+rep.Resource.Name)
			}
		}
		ov.Kinds = append(ov.Kinds, stat)
	}
	// Kind order should match the canonical order regardless of goroutine completion order.
	sort.Slice(ov.Kinds, func(i, j int) bool { return kindRank(ov.Kinds[i].Kind) < kindRank(ov.Kinds[j].Kind) })

	for _, ir := range imageRisk {
		ir.Workloads = dedup(ir.Workloads)
		ov.TopImages = append(ov.TopImages, *ir)
	}
	sort.Slice(ov.TopImages, func(i, j int) bool {
		return riskScore(ov.TopImages[i].Summary) > riskScore(ov.TopImages[j].Summary)
	})
	if len(ov.TopImages) > 8 {
		ov.TopImages = ov.TopImages[:8]
	}
	for ns, t := range nsTotals {
		ov.Namespaces = append(ov.Namespaces, security.NamespaceRisk{Namespace: ns, Totals: t})
	}
	sort.Slice(ov.Namespaces, func(i, j int) bool {
		if a, b := riskScore(ov.Namespaces[i].Totals), riskScore(ov.Namespaces[j].Totals); a != b {
			return a > b
		}
		return ov.Namespaces[i].Namespace < ov.Namespaces[j].Namespace
	})
	return ov, nil
}

// list runs `kubectl get <resource>.group [-A] -o json` and returns the parsed items.
func (q *Querier) list(ctx context.Context, c *domain.Cluster, kc []byte, resource string) ([]reportItem, error) {
	args := []string{"get", resource + "." + security.APIGroup, "-o", "json"}
	if !clusterScoped(resource) {
		args = append(args, "-A")
	}
	res, err := q.ex.Run(ctx, kc, c.ID, args)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		// A cluster that just installed Trivy may not have the CRD registered yet; treat "not found"
		// (no such resource type) as an empty list rather than a hard error, so the page shows "no
		// reports yet" instead of an error while the operator boots.
		if strings.Contains(res.Stderr, "the server doesn't have a resource type") ||
			strings.Contains(res.Stderr, "could not find the requested resource") {
			return nil, nil
		}
		return nil, fmt.Errorf("kubectl get %s: %s", resource, firstLine(res.Stderr))
	}
	var list reportList
	if err := json.Unmarshal(res.Stdout, &list); err != nil {
		return nil, fmt.Errorf("decode %s list: %w", resource, err)
	}
	return list.Items, nil
}

// --- Trivy report CRD wire types --------------------------------------------

type reportList struct {
	Items []reportItem `json:"items"`
}

type reportItem struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Report reportBody `json:"report"`
}

type reportBody struct {
	UpdateTimestamp string `json:"updateTimestamp"`
	Scanner         struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"scanner"`
	Registry *struct {
		Server string `json:"server"`
	} `json:"registry"`
	Artifact *struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
	} `json:"artifact"`
	Summary         summaryJSON  `json:"summary"`
	Vulnerabilities []vulnJSON   `json:"vulnerabilities"`
	Secrets         []secretJSON `json:"secrets"`
	Checks          []checkJSON  `json:"checks"`
}

type summaryJSON struct {
	CriticalCount int `json:"criticalCount"`
	HighCount     int `json:"highCount"`
	MediumCount   int `json:"mediumCount"`
	LowCount      int `json:"lowCount"`
	UnknownCount  int `json:"unknownCount"`
}

type vulnJSON struct {
	VulnerabilityID  string  `json:"vulnerabilityID"`
	Resource         string  `json:"resource"`
	InstalledVersion string  `json:"installedVersion"`
	FixedVersion     string  `json:"fixedVersion"`
	Severity         string  `json:"severity"`
	Title            string  `json:"title"`
	PrimaryLink      string  `json:"primaryLink"`
	Score            float64 `json:"score"`
}

type secretJSON struct {
	RuleID   string `json:"ruleID"`
	Target   string `json:"target"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Match    string `json:"match"`
}

type checkJSON struct {
	CheckID     string   `json:"checkID"`
	Title       string   `json:"title"`
	Severity    string   `json:"severity"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Remediation string   `json:"remediation"`
	Success     bool     `json:"success"`
	Messages    []string `json:"messages"`
}

// report builds the summary row (no findings) from a CR.
func (it reportItem) report(kind security.Kind) security.Report {
	l := it.Metadata.Labels
	r := security.Report{
		Kind:      kind,
		Name:      it.Metadata.Name,
		Namespace: it.Metadata.Namespace,
		Resource: security.Resource{
			Kind:      l["trivy-operator.resource.kind"],
			Name:      l["trivy-operator.resource.name"],
			Container: l["trivy-operator.container.name"],
		},
		Summary:   summaryOf(it.Report.Summary),
		UpdatedAt: it.Report.UpdateTimestamp,
	}
	if it.Report.Scanner.Name != "" {
		r.Scanner = strings.TrimSpace(it.Report.Scanner.Name + " " + it.Report.Scanner.Version)
	}
	if a := it.Report.Artifact; a != nil && a.Repository != "" {
		art := &security.Artifact{Repository: a.Repository, Tag: a.Tag, Digest: a.Digest}
		if it.Report.Registry != nil {
			art.Registry = it.Report.Registry.Server
		}
		r.Artifact = art
	}
	return r
}

// detail builds a full report (summary + findings) from a CR.
func (it reportItem) detail(kind security.Kind) security.ReportDetail {
	d := security.ReportDetail{Report: it.report(kind)}
	switch kind {
	case security.KindVulnerability:
		for _, v := range it.Report.Vulnerabilities {
			d.Findings = append(d.Findings, security.Finding{
				ID: v.VulnerabilityID, Severity: normSeverity(v.Severity), Title: v.Title,
				Resource: v.Resource, InstalledVersion: v.InstalledVersion, FixedVersion: v.FixedVersion,
				Score: v.Score, Link: v.PrimaryLink,
			})
		}
	case security.KindExposedSecret:
		for _, s := range it.Report.Secrets {
			d.Findings = append(d.Findings, security.Finding{
				ID: s.RuleID, Severity: normSeverity(s.Severity), Title: s.Title,
				Category: s.Category, Target: s.Target, Match: s.Match,
			})
		}
	case security.KindConfigAudit, security.KindRbacAssessment:
		for _, ch := range it.Report.Checks {
			if ch.Success {
				continue // only failed checks are findings
			}
			f := security.Finding{
				ID: ch.CheckID, Severity: normSeverity(ch.Severity), Title: ch.Title,
				Category: ch.Category, Description: ch.Description, Remediation: ch.Remediation,
			}
			if f.Description == "" && len(ch.Messages) > 0 {
				f.Description = ch.Messages[0]
			}
			d.Findings = append(d.Findings, f)
		}
	}
	sortFindings(d.Findings)
	return d
}

// --- helpers ----------------------------------------------------------------

func summaryOf(s summaryJSON) security.Counts {
	return security.Counts{Critical: s.CriticalCount, High: s.HighCount, Medium: s.MediumCount, Low: s.LowCount, Unknown: s.UnknownCount}
}

func normSeverity(s string) security.Severity {
	switch strings.ToUpper(s) {
	case "CRITICAL":
		return security.SevCritical
	case "HIGH":
		return security.SevHigh
	case "MEDIUM":
		return security.SevMedium
	case "LOW":
		return security.SevLow
	default:
		return security.SevUnknown
	}
}

func riskScore(c security.Counts) int {
	return c.Critical*1000 + c.High*100 + c.Medium*10 + c.Low
}

func sevRank(s security.Severity) int {
	for i, sv := range security.Severities {
		if sv == s {
			return i
		}
	}
	return len(security.Severities)
}

func kindRank(k security.Kind) int {
	for i, kk := range security.AllKinds {
		if kk == k {
			return i
		}
	}
	return len(security.AllKinds)
}

func sortReports(r []security.Report) {
	sort.Slice(r, func(i, j int) bool {
		if a, b := riskScore(r[i].Summary), riskScore(r[j].Summary); a != b {
			return a > b
		}
		if r[i].Namespace != r[j].Namespace {
			return r[i].Namespace < r[j].Namespace
		}
		return r[i].Name < r[j].Name
	})
}

func sortFindings(f []security.Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if a, b := sevRank(f[i].Severity), sevRank(f[j].Severity); a != b {
			return a < b
		}
		return f[i].ID < f[j].ID
	})
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no output"
	}
	return s
}
