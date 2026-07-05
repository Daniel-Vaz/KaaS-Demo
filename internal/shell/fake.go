package shell

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Fake is the in-process pseudo-terminal used in fake mode (make up-fake): no OS PTY, no bash, no
// real cluster - it emulates a terminal (prompt, echo, line editing, history) and answers a small
// set of kubectl commands synthesized from the cluster's control-plane state. This keeps the portal
// terminal fully demoable without KVM, mirroring every other fake seam. It intentionally does not
// spawn a shell, so the distroless API image stays shell-free.
type Fake struct{}

// NewFake returns the fake shell backend.
func NewFake() *Fake { return &Fake{} }

func (f *Fake) Serve(ctx context.Context, c *domain.Cluster, _ []byte, readOnly bool, term Conn) error {
	s := &fakeSession{term: term, c: c, readOnly: readOnly}
	return s.run(ctx)
}

// fakeSession binds the shared terminal Emulator to this seam's kubectl command synthesis. The line
// editing, history and escape parsing all live in Emulator (emulator.go); this only supplies the
// banner, prompt and per-command rendering.
type fakeSession struct {
	term     Conn
	c        *domain.Cluster
	readOnly bool // read-role viewer: simulate the RBAC that the real cluster would enforce
}

func (s *fakeSession) run(ctx context.Context) error {
	return (&Emulator{
		Term:   s.term,
		Banner: fakeBanner(s.c, s.readOnly),
		Prompt: func() string { return fakePrompt(s.c) },
		Render: func(line string) (string, bool) {
			out := renderCommand(s.c, line, s.readOnly)
			if out == cmdClear {
				return "", true // the `clear` builtin - Emulator emits the screen-clear escape
			}
			return out, false
		},
	}).Run(ctx)
}

// cmdClear is the sentinel renderCommand returns for the `clear` builtin; the fakeSession adapter
// translates it into the Emulator's clear signal.
const cmdClear = "\x00clear\x00"

func fakePrompt(c *domain.Cluster) string {
	// green user@cluster, blue cwd - a familiar shell prompt.
	return fmt.Sprintf("\x1b[1;32mkaas@%s\x1b[0m:\x1b[1;34m~\x1b[0m$ ", c.Name)
}

func fakeBanner(c *domain.Cluster, readOnly bool) string {
	access := ""
	if readOnly {
		access = "Read-only session: you have view access to this cluster - mutating commands are rejected.\n"
	}
	return crlf(fmt.Sprintf(
		"KaaS demo shell - simulated kubectl for cluster %q (bundle %s, k8s v%s).\n"+
			"%s"+
			"Fake mode: output is synthesized from control-plane state, not a live cluster;\n"+
			"real kubectl against real nodes runs in `make up`. Type `help` for modeled commands.\n\n",
		c.Name, c.Bundle, c.K8sVersion, access))
}

// ---- command synthesis (pure: cluster state + command line -> output text) ----

// renderCommand produces the fake response for one command line. Output uses \n line endings (the
// session converts to \r\n). It returns cmdClear for the `clear` builtin so the caller can emit the
// screen-clear escape instead of printing text. When readOnly, mutating kubectl verbs are refused
// with an RBAC-style Forbidden error - simulating what the viewer kubeconfig's RBAC enforces on a
// real cluster.
func renderCommand(c *domain.Cluster, line string, readOnly bool) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case "help":
		return fakeHelp()
	case "clear":
		return cmdClear
	case "exit", "logout":
		return "This is a simulated shell - close the Terminal tab to end the session."
	case "kubectl", "k":
		return renderKubectl(c, fields[1:], readOnly)
	default:
		return fmt.Sprintf("%s: only kubectl is modeled in the fake shell - type `help`.", fields[0])
	}
}

// mutatingVerbs are kubectl subcommands that write to the cluster, so the read-only viewer role
// forbids them (this mirrors the RBAC the real viewer kubeconfig is bound to).
var mutatingVerbs = map[string]bool{
	"create": true, "apply": true, "delete": true, "edit": true, "patch": true,
	"replace": true, "scale": true, "annotate": true, "label": true, "set": true,
	"rollout": true, "drain": true, "cordon": true, "uncordon": true, "taint": true,
	"expose": true, "run": true, "autoscale": true, "exec": true, "attach": true,
}

// forbiddenReadOnly returns kubectl's RBAC-Forbidden message for a mutating verb attempted under the
// read-only viewer role.
func forbiddenReadOnly(verb string) string {
	return fmt.Sprintf(
		"Error from server (Forbidden): \"kubectl %s\" is not permitted - you have read-only (view) "+
			"access to this cluster.", verb)
}

func fakeHelp() string {
	return strings.Join([]string{
		"Modeled commands (fake mode):",
		"  kubectl get nodes [-o wide]     list cluster nodes",
		"  kubectl get pods [-A]           list pods (all namespaces with -A)",
		"  kubectl get namespaces          list namespaces",
		"  kubectl get svc [-A]            list services",
		"  kubectl cluster-info            control-plane endpoints",
		"  kubectl version                 client/server versions",
		"  kubectl config current-context  active context",
		"  clear                           clear the screen",
		"Anything else prints a note - real kubectl runs against a live cluster in `make up`.",
	}, "\n")
}

func renderKubectl(c *domain.Cluster, args []string, readOnly bool) string {
	// Strip flags for verb/resource matching; remember the ones we care about.
	allNS := hasFlag(args, "-A", "--all-namespaces")
	wide := flagValue(args, "-o", "--output") == "wide"
	pos := positional(args)

	if len(pos) == 0 {
		return "kubectl controls the Kubernetes cluster manager. Try `kubectl get nodes`."
	}
	if readOnly && mutatingVerbs[pos[0]] {
		return forbiddenReadOnly(pos[0])
	}
	switch pos[0] {
	case "version":
		return fakeVersion(c)
	case "cluster-info":
		return fakeClusterInfo(c)
	case "config":
		if len(pos) > 1 && pos[1] == "current-context" {
			return "kubernetes-admin@" + c.Name
		}
		return notModeled("kubectl " + strings.Join(pos, " "))
	case "get":
		if len(pos) < 2 {
			return "error: You must specify the type of resource to get."
		}
		switch normalizeResource(pos[1]) {
		case "nodes":
			return fakeGetNodes(c, wide)
		case "pods":
			return fakeGetPods(c, allNS)
		case "namespaces":
			return fakeGetNamespaces(c)
		case "services":
			return fakeGetServices(c, allNS)
		default:
			return notModeled("kubectl get " + pos[1])
		}
	default:
		return notModeled("kubectl " + pos[0])
	}
}

func notModeled(cmd string) string {
	return fmt.Sprintf("simulated shell: %q is not modeled in fake mode (real kubectl runs in `make up`).", cmd)
}

func fakeVersion(c *domain.Cluster) string {
	v := "v" + c.K8sVersion
	return fmt.Sprintf("Client Version: %s\nKustomize Version: v5.4.2\nServer Version: %s", v, v)
}

func fakeClusterInfo(c *domain.Cluster) string {
	host := controlPlaneHost(c)
	return fmt.Sprintf(
		"\x1b[32mKubernetes control plane\x1b[0m is running at \x1b[33mhttps://%s\x1b[0m\n"+
			"\x1b[32mCoreDNS\x1b[0m is running at \x1b[33mhttps://%s/api/v1/namespaces/kube-system/services/kube-dns:dns/proxy\x1b[0m\n\n"+
			"To further debug and diagnose cluster problems, use 'kubectl cluster-info dump'.",
		host, host)
}

func fakeGetNodes(c *domain.Cluster, wide bool) string {
	nodes := c.Nodes
	if len(nodes) == 0 {
		return "No resources found"
	}
	age := clusterAge(c)
	ver := "v" + c.K8sVersion
	var rows [][]string
	header := []string{"NAME", "STATUS", "ROLES", "AGE", "VERSION"}
	if wide {
		header = append(header, "INTERNAL-IP", "OS-IMAGE", "KERNEL-VERSION", "CONTAINER-RUNTIME")
	}
	rows = append(rows, header)
	for _, n := range nodes {
		role := "<none>"
		if n.Role == domain.RoleControlPlane {
			role = "control-plane"
		}
		row := []string{n.VMName, "Ready", role, age, ver}
		if wide {
			row = append(row, n.IP, osImagePretty(c.OSImage), "6.8.0-45-generic", "containerd://1.7.22")
		}
		rows = append(rows, row)
	}
	return table(rows)
}

func fakeGetPods(c *domain.Cluster, allNS bool) string {
	if !allNS {
		return "No resources found in default namespace."
	}
	age := clusterAge(c)
	rows := [][]string{{"NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "AGE"}}
	add := func(ns, name string) {
		rows = append(rows, []string{ns, name, "1/1", "Running", "0", age})
	}
	var cps, workers []domain.Node
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			cps = append(cps, n)
		} else {
			workers = append(workers, n)
		}
	}
	// Control-plane static pods, one set per control-plane node.
	for _, cp := range cps {
		add("kube-system", "etcd-"+cp.VMName)
		add("kube-system", "kube-apiserver-"+cp.VMName)
		add("kube-system", "kube-controller-manager-"+cp.VMName)
		add("kube-system", "kube-scheduler-"+cp.VMName)
	}
	// coredns (2 replicas) + per-node kube-proxy and CNI agent.
	add("kube-system", "coredns-"+randSuffix("7c9", 0)+"-"+randSuffix("ab", 0))
	add("kube-system", "coredns-"+randSuffix("7c9", 1)+"-"+randSuffix("cd", 1))
	cni := c.CNI
	if cni == "" {
		cni = "cni"
	}
	for i, n := range c.Nodes {
		add("kube-system", "kube-proxy-"+randSuffix(n.VMName, i))
		add("kube-system", cni+"-"+randSuffix(n.VMName, i+7))
	}
	// Add-ons that run pods (skip the CNI add-on, already represented above).
	for _, a := range c.Addons {
		if a.Phase == "removing" || a.Name == c.CNI {
			continue
		}
		ns := addonNamespace(a.Name)
		add(ns, a.Name+"-"+randSuffix(a.Name, len(a.Name)))
	}
	return table(rows)
}

func fakeGetNamespaces(c *domain.Cluster) string {
	age := clusterAge(c)
	names := []string{"default", "kube-node-lease", "kube-public", "kube-system"}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, a := range c.Addons {
		if a.Phase == "removing" {
			continue
		}
		if ns := addonNamespace(a.Name); !seen[ns] {
			seen[ns] = true
			names = append(names, ns)
		}
	}
	sort.Strings(names)
	rows := [][]string{{"NAME", "STATUS", "AGE"}}
	for _, n := range names {
		rows = append(rows, []string{n, "Active", age})
	}
	return table(rows)
}

func fakeGetServices(c *domain.Cluster, allNS bool) string {
	age := clusterAge(c)
	rows := [][]string{{"NAMESPACE", "NAME", "TYPE", "CLUSTER-IP", "EXTERNAL-IP", "PORT(S)", "AGE"}}
	if !allNS {
		rows[0] = rows[0][1:] // drop NAMESPACE column for the default namespace view
		rows = append(rows, []string{"kubernetes", "ClusterIP", "10.96.0.1", "<none>", "443/TCP", age})
		return table(rows)
	}
	rows = append(rows, []string{"default", "kubernetes", "ClusterIP", "10.96.0.1", "<none>", "443/TCP", age})
	rows = append(rows, []string{"kube-system", "kube-dns", "ClusterIP", "10.96.0.10", "<none>", "53/UDP,53/TCP,9153/TCP", age})
	for _, a := range c.Addons {
		if a.Phase == "removing" || a.Name == c.CNI {
			continue
		}
		rows = append(rows, []string{addonNamespace(a.Name), a.Name, "ClusterIP", "10.96." + fmt.Sprint(100+len(a.Name)%50) + ".7", "<none>", "443/TCP", age})
	}
	return table(rows)
}

// ---- small formatting helpers ----

// table renders rows (first row is the header) as space-padded columns, kubectl-style.
func table(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	for _, r := range rows {
		for i, cell := range r {
			if i == len(r)-1 {
				b.WriteString(cell) // no trailing pad on the last column
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+3))
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// flagValue returns the value of -o/--output style flags in either "-o wide" or "-o=wide" form.
func flagValue(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return strings.TrimPrefix(a, n+"=")
			}
		}
	}
	return ""
}

// positional returns the non-flag arguments (and drops values consumed by -o/--output).
func positional(args []string) []string {
	var out []string
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == "-o" || a == "--output" {
			skip = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// normalizeResource maps kubectl's resource aliases to a canonical plural.
func normalizeResource(r string) string {
	switch r {
	case "no", "node", "nodes":
		return "nodes"
	case "po", "pod", "pods":
		return "pods"
	case "ns", "namespace", "namespaces":
		return "namespaces"
	case "svc", "service", "services":
		return "services"
	default:
		return r
	}
}

func controlPlaneHost(c *domain.Cluster) string {
	if ep := c.APIEndpoint(); ep != "" {
		return ep
	}
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane && n.IP != "" {
			return n.IP + ":6443"
		}
	}
	return "control-plane:6443"
}

func addonNamespace(name string) string {
	switch name {
	case "metrics-server", "kube-proxy":
		return "kube-system"
	case "ingress-nginx":
		return "ingress-nginx"
	default:
		return "kube-system"
	}
}

func osImagePretty(os string) string {
	// "ubuntu-26.04" -> "Ubuntu 26.04 LTS"
	parts := strings.SplitN(os, "-", 2)
	if len(parts) == 2 {
		return "Ubuntu " + parts[1] + " LTS"
	}
	return os
}

// clusterAge renders time since the cluster was created in kubectl's compact form (e.g. 12m, 3h5m).
func clusterAge(c *domain.Cluster) string {
	d := time.Since(c.CreatedAt)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// randSuffix returns a short deterministic hex-ish suffix seeded by s+i, so synthesized pod names
// look real and stay stable across redraws within a session (no crypto needs - cosmetic only).
func randSuffix(s string, i int) string {
	const hex = "0123456789abcdef"
	h := uint32(2166136261)
	for _, r := range s {
		h = (h ^ uint32(r)) * 16777619
	}
	h ^= uint32(i * 2654435761)
	var b [5]byte
	for j := range b {
		b[j] = hex[h&0xf]
		h >>= 4
	}
	return string(b[:])
}
