package security

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Fake is the in-process Querier used in fake mode (make up-fake): it synthesizes a plausible,
// deterministic set of Trivy reports from a cluster's identity - image CVEs, workload
// misconfigurations, exposed secrets, and over-permissive RBAC - so the whole Security page renders
// with no real Trivy Operator, mirroring every other fake seam. Deterministic in the cluster id +
// target, so a report's list-row summary and its detail always agree.
type Fake struct{}

// NewFake returns the fake security querier.
func NewFake() *Fake { return &Fake{} }

// workloadTarget is a canned scanned workload the fake generates reports for. risk scales its
// vulnerability load (0 = pristine, 3 = a neglected legacy image); secret marks images the fake bakes
// an exposed credential into.
type workloadTarget struct {
	ns, kind, name, container string
	registry, repo, tag       string
	risk                      int
	secret                    bool
}

// roleTarget is a canned Role/ClusterRole the fake generates an RbacAssessmentReport for.
type roleTarget struct {
	ns, kind, name string
	risk           int
}

// fakeWorkloads is the canned cluster the fake scans: a realistic spread of system add-ons (mostly
// clean) and application workloads (older images carry more CVEs).
var fakeWorkloads = []workloadTarget{
	{"kube-system", "DaemonSet", "cilium", "cilium-agent", "quay.io", "cilium/cilium", "v1.19.5", 0, false},
	{"kube-system", "Deployment", "coredns", "coredns", "registry.k8s.io", "coredns/coredns", "v1.11.3", 0, false},
	{"monitoring-system", "Deployment", "kube-prometheus-stack-grafana", "grafana", "docker.io", "grafana/grafana", "11.4.0", 1, false},
	{"monitoring-system", "StatefulSet", "prometheus-kube-prometheus-stack-prometheus", "prometheus", "quay.io", "prometheus/prometheus", "v3.1.0", 0, false},
	{"trivy-system", "Deployment", "trivy-operator", "trivy-operator", "ghcr.io", "aquasecurity/trivy-operator", "0.24.0", 1, false},
	{"default", "Deployment", "storefront", "nginx", "docker.io", "library/nginx", "1.27.3", 1, false},
	{"default", "Deployment", "checkout-api", "app", "docker.io", "library/node", "18.20.4", 2, true},
	{"default", "Deployment", "cache", "redis", "docker.io", "library/redis", "7.4.1", 1, false},
	{"payments", "Deployment", "billing", "billing", "docker.io", "library/python", "3.8-slim", 3, true},
	{"payments", "StatefulSet", "ledger", "postgres", "docker.io", "library/postgres", "14.10", 2, false},
}

// fakeRoles is the canned RBAC surface the fake assesses.
var fakeRoles = []roleTarget{
	{"default", "Role", "storefront-reader", 0},
	{"payments", "Role", "billing-admin", 2},
	{"kube-system", "Role", "leader-election", 0},
	{"", "ClusterRole", "cluster-debugger", 3},
}

func (Fake) Overview(_ context.Context, c *domain.Cluster, _ []byte) (*Overview, error) {
	ov := &Overview{GeneratedAt: now()}
	nsTotals := map[string]Counts{}
	imageRisk := map[string]*ImageRisk{}

	for _, k := range AllKinds {
		reports := fakeReportsFor(c, k)
		stat := KindStat{Kind: k, Title: kindMetas[k].Title, ReportCount: len(reports)}
		for _, r := range reports {
			stat.Totals = stat.Totals.Plus(r.Summary)
			nsTotals[r.Namespace] = nsTotals[r.Namespace].Plus(r.Summary)
			if k == KindVulnerability && r.Artifact != nil {
				img := r.Artifact.Image()
				ir := imageRisk[img]
				if ir == nil {
					ir = &ImageRisk{Image: img}
					imageRisk[img] = ir
				}
				ir.Summary = ir.Summary.Plus(r.Summary)
				ir.Workloads = append(ir.Workloads, r.Namespace+"/"+r.Resource.Name)
			}
		}
		ov.Kinds = append(ov.Kinds, stat)
	}

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
		ov.Namespaces = append(ov.Namespaces, NamespaceRisk{Namespace: ns, Totals: t})
	}
	sort.Slice(ov.Namespaces, func(i, j int) bool {
		if a, b := riskScore(ov.Namespaces[i].Totals), riskScore(ov.Namespaces[j].Totals); a != b {
			return a > b
		}
		return ov.Namespaces[i].Namespace < ov.Namespaces[j].Namespace
	})
	return ov, nil
}

func (Fake) Reports(_ context.Context, c *domain.Cluster, _ []byte, kind Kind) ([]Report, error) {
	if _, ok := kindMetas[kind]; !ok {
		return nil, ErrUnknownKind
	}
	details := fakeReportsFor(c, kind)
	out := make([]Report, 0, len(details))
	for _, d := range details {
		out = append(out, d.Report)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := riskScore(out[i].Summary), riskScore(out[j].Summary); a != b {
			return a > b
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (Fake) Report(_ context.Context, c *domain.Cluster, _ []byte, kind Kind, namespace, name string) (*ReportDetail, error) {
	if _, ok := kindMetas[kind]; !ok {
		return nil, ErrUnknownKind
	}
	for _, d := range fakeReportsFor(c, kind) {
		if d.Namespace == namespace && d.Name == name {
			out := d
			return &out, nil
		}
	}
	return nil, fmt.Errorf("security: report %s/%s (%s) not found", namespace, name, kind)
}

// fakeReportsFor builds every report of a kind for the cluster, each with its full finding list (the
// summary is derived from the findings, so list and detail can never disagree).
func fakeReportsFor(c *domain.Cluster, kind Kind) []ReportDetail {
	var out []ReportDetail
	switch kind {
	case KindRbacAssessment:
		for _, r := range fakeRoles {
			out = append(out, fakeRbacReport(c, r))
		}
	case KindExposedSecret:
		for _, w := range fakeWorkloads {
			if !w.secret {
				continue // Trivy only writes an ExposedSecretReport when it finds something
			}
			out = append(out, fakeExposedSecretReport(c, w))
		}
	case KindConfigAudit:
		for _, w := range fakeWorkloads {
			out = append(out, fakeConfigAuditReport(c, w))
		}
	case KindVulnerability:
		for _, w := range fakeWorkloads {
			out = append(out, fakeVulnReport(c, w))
		}
	}
	return out
}

func (w workloadTarget) artifact() *Artifact {
	return &Artifact{Registry: w.registry, Repository: w.repo, Tag: w.tag,
		Digest: "sha256:" + hex12(w.repo+w.tag)}
}

func (w workloadTarget) resource() Resource {
	return Resource{Kind: w.kind, Name: w.name, Container: w.container}
}

func baseReport(c *domain.Cluster, kind Kind, ns, crName string) Report {
	return Report{
		Kind: kind, Namespace: ns, Name: crName,
		Scanner:   "Trivy 0.58.1",
		UpdatedAt: driftTime(c.ID + crName),
	}
}

// fakeVulnReport synthesizes a VulnerabilityReport: a CVE list scaled by the target's risk, drifting
// slowly so the page feels live under polling.
func fakeVulnReport(c *domain.Cluster, w workloadTarget) ReportDetail {
	seed := c.ID + "|vuln|" + w.ns + "|" + w.name + "|" + w.container
	crName := fmt.Sprintf("%s-%s-%s", lower(w.kind), w.name, w.container)
	r := baseReport(c, KindVulnerability, w.ns, crName)
	r.Resource, r.Artifact = w.resource(), w.artifact()

	// Severity budget grows with risk; a slow time term nudges counts so refreshes shift a little.
	drift := driftInt(seed, 2)
	want := Counts{
		Critical: clampMin(w.risk-1+driftInt(seed+"c", 2), 0),
		High:     w.risk + driftInt(seed+"h", 2),
		Medium:   w.risk*2 + 1 + driftInt(seed+"m", 3),
		Low:      w.risk*3 + 2 + drift,
	}
	var findings []Finding
	for _, sev := range []Severity{SevCritical, SevHigh, SevMedium, SevLow} {
		for i := 0; i < want.getSev(sev); i++ {
			findings = append(findings, fakeCVE(seed, sev, i))
		}
	}
	sortFindings(findings)
	r.Summary = summarize(findings)
	return ReportDetail{Report: r, Findings: findings}
}

// fakeConfigAuditReport synthesizes a ConfigAuditReport: a handful of failed best-practice checks,
// how many scaled by risk.
func fakeConfigAuditReport(c *domain.Cluster, w workloadTarget) ReportDetail {
	seed := c.ID + "|conf|" + w.ns + "|" + w.name
	crName := fmt.Sprintf("%s-%s", lower(w.kind), w.name)
	r := baseReport(c, KindConfigAudit, w.ns, crName)
	r.Resource = Resource{Kind: w.kind, Name: w.name}

	n := 2 + w.risk + driftInt(seed, 2)
	if n > len(configChecks) {
		n = len(configChecks)
	}
	start := int(hash(seed)) % len(configChecks)
	var findings []Finding
	for i := 0; i < n; i++ {
		findings = append(findings, configChecks[(start+i)%len(configChecks)].finding())
	}
	sortFindings(findings)
	r.Summary = summarize(findings)
	return ReportDetail{Report: r, Findings: findings}
}

// fakeExposedSecretReport synthesizes an ExposedSecretReport for a target the fake baked a secret
// into (Trivy redacts the matched value, which the fake mirrors).
func fakeExposedSecretReport(c *domain.Cluster, w workloadTarget) ReportDetail {
	seed := c.ID + "|secret|" + w.ns + "|" + w.name
	crName := fmt.Sprintf("%s-%s-%s", lower(w.kind), w.name, w.container)
	r := baseReport(c, KindExposedSecret, w.ns, crName)
	r.Resource, r.Artifact = w.resource(), w.artifact()

	n := 1 + driftInt(seed, 2)
	if n > len(secretRules) {
		n = len(secretRules)
	}
	start := int(hash(seed)) % len(secretRules)
	var findings []Finding
	for i := 0; i < n; i++ {
		findings = append(findings, secretRules[(start+i)%len(secretRules)].finding(i))
	}
	sortFindings(findings)
	r.Summary = summarize(findings)
	return ReportDetail{Report: r, Findings: findings}
}

// fakeRbacReport synthesizes an RbacAssessmentReport for a Role/ClusterRole.
func fakeRbacReport(c *domain.Cluster, role roleTarget) ReportDetail {
	seed := c.ID + "|rbac|" + role.ns + "|" + role.name
	crName := fmt.Sprintf("%s-%s", lower(role.kind), role.name)
	r := baseReport(c, KindRbacAssessment, role.ns, crName)
	r.Resource = Resource{Kind: role.kind, Name: role.name}

	n := role.risk + driftInt(seed, 2)
	if n > len(rbacChecks) {
		n = len(rbacChecks)
	}
	start := int(hash(seed)) % len(rbacChecks)
	var findings []Finding
	for i := 0; i < n; i++ {
		findings = append(findings, rbacChecks[(start+i)%len(rbacChecks)].finding())
	}
	sortFindings(findings)
	r.Summary = summarize(findings)
	return ReportDetail{Report: r, Findings: findings}
}

// --- finding template pools -------------------------------------------------

type cveTemplate struct {
	id, pkg, title, fixed, link string
	score                       float64
}

// cvePools maps a severity to a pool of realistic-looking CVE templates the fake draws from.
var cvePools = map[Severity][]cveTemplate{
	SevCritical: {
		{"CVE-2024-3094", "xz-utils", "Backdoor in liblzma via malicious build", "5.6.1", "https://avd.aquasec.com/nvd/cve-2024-3094", 10.0},
		{"CVE-2023-44487", "golang.org/x/net", "HTTP/2 rapid reset denial of service", "0.17.0", "https://avd.aquasec.com/nvd/cve-2023-44487", 9.1},
		{"CVE-2022-42889", "commons-text", "Arbitrary code execution (Text4Shell)", "1.10.0", "https://avd.aquasec.com/nvd/cve-2022-42889", 9.8},
	},
	SevHigh: {
		{"CVE-2023-38545", "libcurl", "SOCKS5 heap buffer overflow", "8.4.0", "https://avd.aquasec.com/nvd/cve-2023-38545", 8.8},
		{"CVE-2024-6387", "openssh", "regreSSHion: RCE in sshd signal handler", "9.8p1", "https://avd.aquasec.com/nvd/cve-2024-6387", 8.1},
		{"CVE-2023-4911", "glibc", "Buffer overflow in ld.so (Looney Tunables)", "2.38-r1", "https://avd.aquasec.com/nvd/cve-2023-4911", 7.8},
	},
	SevMedium: {
		{"CVE-2023-29491", "ncurses", "Memory corruption via crafted terminfo", "6.4-r2", "https://avd.aquasec.com/nvd/cve-2023-29491", 6.5},
		{"CVE-2024-0553", "gnutls", "Timing side-channel in RSA decryption", "3.8.3", "https://avd.aquasec.com/nvd/cve-2024-0553", 6.5},
		{"CVE-2023-5678", "openssl", "DH key generation denial of service", "3.0.12", "https://avd.aquasec.com/nvd/cve-2023-5678", 5.3},
	},
	SevLow: {
		{"CVE-2023-7008", "systemd", "DNSSEC validation bypass", "", "https://avd.aquasec.com/nvd/cve-2023-7008", 3.7},
		{"CVE-2022-3715", "bash", "Out-of-bounds read in valid_parameter_transform", "", "https://avd.aquasec.com/nvd/cve-2022-3715", 2.5},
		{"CVE-2023-45853", "zlib", "Integer overflow in MiniZip", "1.3-r1", "https://avd.aquasec.com/nvd/cve-2023-45853", 3.1},
	},
}

func fakeCVE(seed string, sev Severity, i int) Finding {
	pool := cvePools[sev]
	t := pool[(int(hash(seed+string(sev)))+i)%len(pool)]
	return Finding{
		ID: t.id, Severity: sev, Title: t.title, Resource: t.pkg,
		InstalledVersion: installedVersion(seed + t.id), FixedVersion: t.fixed,
		Score: t.score, Link: t.link,
	}
}

type checkTemplate struct {
	id, title, category, description, remediation string
	sev                                           Severity
}

func (t checkTemplate) finding() Finding {
	return Finding{ID: t.id, Severity: t.sev, Title: t.title, Category: t.category,
		Description: t.description, Remediation: t.remediation}
}

// configChecks are Trivy's built-in Kubernetes misconfiguration checks (KSV… IDs).
var configChecks = []checkTemplate{
	{"KSV014", "Root file system is not read-only", "Kubernetes Security Check", "An immutable root filesystem prevents attackers from writing to the container.", "Set securityContext.readOnlyRootFilesystem to true.", SevLow},
	{"KSV017", "Privileged container", "Kubernetes Security Check", "A privileged container can access all devices on the host.", "Set securityContext.privileged to false.", SevHigh},
	{"KSV020", "Runs with low user ID", "Kubernetes Security Check", "Force the container to run as a high UID to avoid host conflicts.", "Set runAsUser to a value > 10000.", SevLow},
	{"KSV003", "Default capabilities not dropped", "Kubernetes Security Check", "Containers should drop all capabilities and add back only those required.", "Add 'ALL' to securityContext.capabilities.drop.", SevLow},
	{"KSV001", "Process can elevate its own privileges", "Kubernetes Security Check", "allowPrivilegeEscalation should be false.", "Set securityContext.allowPrivilegeEscalation to false.", SevMedium},
	{"KSV011", "CPU not limited", "Kubernetes Security Check", "Enforcing a CPU limit prevents noisy-neighbour exhaustion.", "Set resources.limits.cpu.", SevLow},
	{"KSV018", "Memory not limited", "Kubernetes Security Check", "Enforcing a memory limit prevents OOM of co-scheduled pods.", "Set resources.limits.memory.", SevLow},
	{"KSV012", "Runs as root user", "Kubernetes Security Check", "Container must not run as the root user.", "Set runAsNonRoot to true.", SevMedium},
	{"KSV104", "Seccomp profile is not set", "Kubernetes Security Check", "A seccomp profile restricts the syscalls a container may make.", "Set seccompProfile.type to RuntimeDefault.", SevMedium},
}

type secretTemplate struct {
	ruleID, title, category, target string
	sev                             Severity
}

func (t secretTemplate) finding(i int) Finding {
	return Finding{ID: t.ruleID, Severity: t.sev, Title: t.title, Category: t.category,
		Target: t.target, Match: t.category + ": *****" + fmt.Sprintf("%02d", i) + "*****"}
}

// secretRules are Trivy's built-in exposed-secret rules.
var secretRules = []secretTemplate{
	{"aws-access-key-id", "AWS Access Key ID", "AWS", "/app/.env", SevCritical},
	{"github-pat", "GitHub Personal Access Token", "GitHub", "/root/.netrc", SevHigh},
	{"private-key", "Asymmetric Private Key", "AsymmetricPrivateKey", "/etc/ssl/private/tls.key", SevHigh},
	{"stripe-secret-token", "Stripe Secret Key", "Stripe", "/app/config/secrets.yaml", SevCritical},
}

// rbacChecks are Trivy's built-in RBAC assessment checks.
var rbacChecks = []checkTemplate{
	{"KSV041", "Manage secrets", "Kubernetes Security Check", "Viewing secrets at the cluster scope can lead to privilege escalation.", "Restrict get/list/watch on secrets.", SevCritical},
	{"KSV045", "No wildcard verb and resource roles", "Kubernetes Security Check", "A role granting '*' on '*' is effectively cluster-admin.", "Scope verbs and resources to what is required.", SevCritical},
	{"KSV047", "Do not allow privilege escalation from node proxy", "Kubernetes Security Check", "nodes/proxy allows bypassing the API server's admission control.", "Remove nodes/proxy from the role.", SevHigh},
	{"KSV051", "Manage RBAC resources", "Kubernetes Security Check", "The ability to modify roles/bindings enables privilege escalation.", "Remove write access to rbac.authorization.k8s.io.", SevHigh},
	{"KSV056", "Manage networking resources", "Kubernetes Security Check", "Editing NetworkPolicies/Ingress can expose or reroute traffic.", "Restrict write access to networking resources.", SevMedium},
	{"KSV042", "Delete pods", "Kubernetes Security Check", "Deleting pods can cause denial of service.", "Restrict the delete verb on pods.", SevMedium},
}

// --- helpers ----------------------------------------------------------------

func (c Counts) getSev(s Severity) int {
	switch s {
	case SevCritical:
		return c.Critical
	case SevHigh:
		return c.High
	case SevMedium:
		return c.Medium
	case SevLow:
		return c.Low
	default:
		return c.Unknown
	}
}

func summarize(findings []Finding) Counts {
	var c Counts
	for _, f := range findings {
		c.Add(f.Severity)
	}
	return c
}

// sevRank orders severities most-severe first for stable sorting.
func sevRank(s Severity) int {
	for i, sv := range Severities {
		if sv == s {
			return i
		}
	}
	return len(Severities)
}

func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if a, b := sevRank(f[i].Severity), sevRank(f[j].Severity); a != b {
			return a < b
		}
		return f[i].ID < f[j].ID
	})
}

// riskScore weights a severity breakdown so a single Critical outranks any pile of Lows - the order
// the Overview surfaces "worst first".
func riskScore(c Counts) int {
	return c.Critical*1000 + c.High*100 + c.Medium*10 + c.Low
}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// driftInt returns a small non-negative integer in [0,span) that shifts every few minutes, so polled
// panels aren't perfectly static. Deterministic in the seed within a time bucket.
func driftInt(seed string, span int) int {
	if span <= 0 {
		return 0
	}
	bucket := time.Now().Unix() / 240
	return int(hash(fmt.Sprintf("%s|%d", seed, bucket))) % span
}

func clampMin(v, lo int) int {
	if v < lo {
		return lo
	}
	return v
}

func hex12(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

// installedVersion fabricates a plausible pinned package version.
func installedVersion(seed string) string {
	h := hash(seed)
	return fmt.Sprintf("%d.%d.%d", h%3, h%13, h%29)
}

func lower(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if ch >= 'A' && ch <= 'Z' {
			b[i] = ch + 32
		}
	}
	return string(b)
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

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// driftTime returns a per-report "last scanned" timestamp within the last ~hour, stable per seed.
func driftTime(seed string) string {
	ago := time.Duration(hash(seed)%3600) * time.Second
	return time.Now().Add(-ago).UTC().Format(time.RFC3339)
}
