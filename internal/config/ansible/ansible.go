// Package ansible is the real config.Manager: it forms the kubeadm cluster by
// running the playbooks in ansible/ via `ansible-playbook`, with a dynamic inventory
// generated per run from the cluster's nodes.
//
// The admin kubeconfig and worker join command are shuttled back from the control-plane
// node into a per-cluster artifacts dir (see the controlplane role), which the wrapper
// then reads. All steps are idempotent (kubeadm actions are guarded by readiness checks).
// Output is streamed line-by-line into the cluster event timeline via internal/procstream.
package ansible

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
	"github.com/Daniel-Vaz/KaaS-demo/internal/registry"
)

type Config struct {
	Bin               string       // "ansible-playbook"
	PlaybookDir       string       // abs path to ansible/
	WorkDir           string       // base dir for per-cluster inventory + artifacts
	SSHUser           string       // cloud-init user, default "kaas"
	SSHPrivateKeyFile string       // key matching the public key injected via cloud-init
	SSHCommonArgs     string       // extra ssh flags for every host; a ProxyCommand through a remote KVM host (internal/kvmhost), else ""
	Events            events.Sink  // optional; streams ansible output as events
	Log               *slog.Logger // required

	// EtcdQuotaBytes and EtcdCompactionRetention tune every cluster's etcd backend store: the
	// controlplane_etcd role bakes them into the etcd static pod at kubeadm init/join, and the
	// etcd_maintenance role converges them onto members that predate it. Deployment-level (they
	// come from KAAS_ETCD_QUOTA_BYTES / KAAS_ETCD_COMPACTION_RETENTION), not per cluster - there is
	// no reason for one tenant's etcd to have a different ceiling. Zero/empty falls back to the
	// role's own defaults.
	EtcdQuotaBytes          int64
	EtcdCompactionRetention string

	// Registry is what a cluster node needs in order to pull through the platform's image registry:
	// the CA to trust and the containerd mirror list. It is injected into every playbook's extra-vars
	// (see registryVars) so the registry_trust role runs on the bootstrap path, before the first
	// image pull. Deployment-level and CREDENTIAL-FREE - it is derived from configuration alone, so
	// every worker replica renders the same thing with no coordination. The zero value means this
	// deployment has no registry, and the role does nothing.
	Registry registry.NodeTrust
}

type Manager struct{ cfg Config }

func New(cfg Config) (*Manager, error) {
	if cfg.Bin == "" {
		cfg.Bin = "ansible-playbook"
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = "kaas"
	}
	for name, v := range map[string]string{"PlaybookDir": cfg.PlaybookDir, "WorkDir": cfg.WorkDir, "SSHPrivateKeyFile": cfg.SSHPrivateKeyFile} {
		if v == "" {
			return nil, fmt.Errorf("ansible: %s is required", name)
		}
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("ansible: Log is required")
	}
	return &Manager{cfg: cfg}, nil
}

func (m *Manager) InitControlPlane(ctx context.Context, c *domain.Cluster) ([]byte, string, error) {
	art, err := m.prep(c)
	if err != nil {
		return nil, "", err
	}
	extra := map[string]any{
		"k8s_version":   c.K8sVersion,
		"k8s_minor":     minorStr(c.K8sVersion),
		"pod_cidr":      c.PodCIDR,
		"svc_cidr":      c.SvcCIDR,
		"artifacts_dir": art,
	}
	// HA: the bootstrap play stands up keepalived+haproxy on every control-plane node, inits
	// the first with a shared --control-plane-endpoint (the VIP), and joins the rest as
	// control planes. The VRRP router id is the VIP's last octet - unique because VIPs are.
	if c.HA() {
		extra["ha"] = true
		extra["control_plane_vip"] = c.APIVIP
		extra["control_plane_endpoint"] = c.APIEndpoint()
		extra["vrrp_router_id"] = vridFromVIP(c.APIVIP)
		extra["vrrp_password"] = vrrpPassword(c.ID)
	}
	if err := m.playbook(ctx, c, "bootstrap.yml", extra); err != nil {
		return nil, "", err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(art, "admin.conf"))
	if err != nil {
		return nil, "", fmt.Errorf("ansible: read kubeconfig: %w", err)
	}
	join, err := os.ReadFile(filepath.Join(art, "join.sh"))
	if err != nil {
		return nil, "", fmt.Errorf("ansible: read join command: %w", err)
	}
	return kubeconfig, strings.TrimSpace(string(join)), nil
}

// EnsureViewerKubeconfig applies the read-only viewer RBAC on the first control plane and fetches
// the resulting kubeconfig back to the artifacts dir. The playbook reuses the node's admin.conf to
// apply the objects and to read the cluster CA + server, so the viewer config targets the same
// endpoint (the VIP for HA) with a ServiceAccount token instead of the admin client cert. Idempotent
// (kubectl apply + a deterministic token Secret).
func (m *Manager) EnsureViewerKubeconfig(ctx context.Context, c *domain.Cluster) ([]byte, error) {
	art, err := m.prep(c)
	if err != nil {
		return nil, err
	}
	extra := map[string]any{
		"cluster_name":  c.Name,
		"artifacts_dir": art,
		// The two Kubernetes groups a per-user download binds to (see App.DownloadKubeconfig). Passed
		// from Go rather than hard-coded in the role so domain.KubeGroup* stays the single source shared
		// by the cert's Subject O= and the ClusterRoleBinding subject.
		"writers_group": domain.KubeGroupWriters,
		"readers_group": domain.KubeGroupReaders,
	}
	if err := m.playbook(ctx, c, "viewer-kubeconfig.yml", extra); err != nil {
		return nil, err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(art, "viewer.conf"))
	if err != nil {
		return nil, fmt.Errorf("ansible: read viewer kubeconfig: %w", err)
	}
	return kubeconfig, nil
}

func (m *Manager) JoinWorkers(ctx context.Context, c *domain.Cluster) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	// join.yml mints a fresh join command on the control plane itself (bootstrap tokens
	// expire), so this is safe to run both at create and for later scale-ups.
	extra := map[string]any{
		"k8s_version": c.K8sVersion,
		"k8s_minor":   minorStr(c.K8sVersion),
		// The node-pool label KEY (the value is each worker's own `nodepool` host var). Passed from Go
		// rather than hard-coded in the role so domain.PoolLabel stays its single source.
		"pool_label": domain.PoolLabel,
	}
	// join.yml waits for each joined worker to report Ready before returning, so the reconciler
	// never starts on the next node before this one is genuinely serving. But a node can't go Ready
	// without a CNI, and THIS call site is the one exception where the CNI isn't installed yet: the
	// very first JoinWorkers, from PhaseControlPlaneReady, runs before InstallCNI (the next phase).
	// Every other caller (scale-up from PhaseUpdating, a worker roll from PhaseUpgrading) is only
	// reachable after the cluster has been Ready at least once, so the CNI is already there.
	if c.Phase == domain.PhaseControlPlaneReady {
		extra["wait_ready"] = false
	}
	return m.playbook(ctx, c, "join.yml", extra)
}

// RemoveWorkers drains and deletes the named worker nodes from the cluster (scale-down).
// The play runs from the control plane by node name, so it works even if the target VM is
// already unreachable. Idempotent: `kubectl delete node` tolerates an absent node.
func (m *Manager) RemoveWorkers(ctx context.Context, c *domain.Cluster, workers []domain.Node) error {
	if len(workers) == 0 {
		return nil
	}
	if _, err := m.prep(c); err != nil {
		return err
	}
	names := make([]string, 0, len(workers))
	for _, w := range workers {
		names = append(names, w.VMName)
	}
	extra := map[string]any{"remove_nodes": names}
	return m.playbook(ctx, c, "remove-worker.yml", extra)
}

// The CNI's `helm --wait` budget. Cilium is a DaemonSet and the CNI is installed only once every
// worker has joined (see reconcile.go), so the wait covers the whole cluster and every node pulls
// the agent image at once - the cost grows with the node count, and contended storage stretches it
// further. The old fixed 5m was therefore tightest on exactly the largest clusters, and overrunning
// it left the release wedged in `pending-install`. The cap stays well under
// KAAS_RECONCILE_JOB_TIMEOUT so River's job kill remains the outer backstop rather than the first
// thing to fire.
const (
	cniTimeoutBase    = 5 * time.Minute
	cniTimeoutPerNode = 90 * time.Second
	cniTimeoutMax     = 30 * time.Minute
)

// cniTimeout scales the CNI's helm --timeout with the cluster's desired node count, via
// domain.DesiredNodes - the single source of the desired node set, so this can't drift from the
// set of nodes the agent actually has to land on.
func cniTimeout(c *domain.Cluster) string {
	d := cniTimeoutBase + time.Duration(len(domain.DesiredNodes(c)))*cniTimeoutPerNode
	return min(d, cniTimeoutMax).String()
}

// cniOperatorReplicas gives an HA control plane an HA Cilium operator - as a Deployment, a lone
// replica is the single point of failure HA is meant to remove. Cilium's operator carries a
// required hostname anti-affinity, so 2 replicas need 2 schedulable nodes; HA means at least 3
// control planes, so that always holds.
func cniOperatorReplicas(c *domain.Cluster) int {
	if c.HA() {
		return 2
	}
	return 1
}

// cniVars builds the extra-vars every cni.yml run needs - the initial install and the later
// ServiceMonitor re-run alike. Shared so the two callers can't drift on the tunables: a re-run
// that dropped the scaled timeout or the operator replica count would silently re-render the
// release with different values than it was installed with.
func cniVars(c *domain.Cluster) map[string]any {
	extra := map[string]any{
		"cni":                   c.CNI,
		"pod_cidr":              c.PodCIDR,
		"cni_timeout":           cniTimeout(c),
		"cni_operator_replicas": cniOperatorReplicas(c),
	}
	// Pass the bundle-pinned CNI version as provenance; omit it for older clusters that
	// predate the field so the role's built-in default still applies.
	if c.CNIVersion != "" {
		extra["cni_version"] = c.CNIVersion
	}
	return extra
}

func (m *Manager) InstallCNI(ctx context.Context, c *domain.Cluster) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	return m.playbook(ctx, c, "cni.yml", cniVars(c))
}

// EnsureCNIMetrics re-runs cni.yml with cni_service_monitors=true, so the CNI's helm release is
// upgraded to publish Prometheus ServiceMonitors (agent + operator). Only meaningful once the
// monitoring stack's ServiceMonitor CRD exists - the reconciler gates the call on that. Idempotent:
// the cni role uses `helm upgrade --install`, so this is a no-op change on a cluster already wired.
func (m *Manager) EnsureCNIMetrics(ctx context.Context, c *domain.Cluster) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	extra := cniVars(c)
	extra["cni_service_monitors"] = true
	return m.playbook(ctx, c, "cni.yml", extra)
}

// EnsureControlPlaneMetrics runs control-plane-metrics.yml: fixes kubeadm's non-monitoring-friendly
// defaults (loopback-bound scheduler/controller-manager/kube-proxy metrics, no unauthenticated etcd
// metrics listener) so kube-prometheus-stack's built-in ServiceMonitors for these components can
// actually scrape them. No cluster-specific extra-vars needed - see the playbook/role for detail.
func (m *Manager) EnsureControlPlaneMetrics(ctx context.Context, c *domain.Cluster) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	return m.playbook(ctx, c, "control-plane-metrics.yml", map[string]any{})
}

// EnsureDefaultGateway runs default-gateway.yml: applies the default MetalLB L2 pool
// (IPAddressPool/L2Advertisement built from the cluster's reserved LoadBalancerIP) and the Envoy
// GatewayClass + default Gateway pinned to it. Runs once the metallb + envoy-gateway add-ons are
// installed (the reconciler gates on that). Idempotent (`kubectl apply` on the first control plane).
//
// When cert-manager is also on the cluster it makes north-south routes HTTPS-ready by default: the
// role additionally applies a self-signed ClusterIssuer, and - when the cluster owns an apps domain -
// a wildcard certificate for "*.<apps domain>" plus a TLS listener on the default Gateway that
// terminates it. The apps domain comes from admission (c.AppsDomain), so it is known here even before
// the wildcard DNS record is published (reconcileDNSWiring runs after this).
func (m *Manager) EnsureDefaultGateway(ctx context.Context, c *domain.Cluster) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	return m.playbook(ctx, c, "default-gateway.yml", map[string]any{
		"load_balancer_ip": c.LoadBalancerIP,
		"cert_manager":     clusterHasLiveAddon(c, "cert-manager"),
		"apps_domain":      c.AppsDomain,
	})
}

// clusterHasLiveAddon reports whether the cluster carries an add-on that is not being removed - the
// question is whether the chart is part of the cluster's shape, mirroring the reconciler's gate.
func clusterHasLiveAddon(c *domain.Cluster, name string) bool {
	for _, a := range c.Addons {
		if a.Name == name && a.Phase != "removing" {
			return true
		}
	}
	return false
}

// CertExpiry runs check-cert-expiration.yml (read-only) and reads the earliest control-plane
// certificate expiry the renew_certs role wrote to the artifacts dir (Unix epoch seconds).
func (m *Manager) CertExpiry(ctx context.Context, c *domain.Cluster) (time.Time, error) {
	art, err := m.prep(c)
	if err != nil {
		return time.Time{}, err
	}
	if err := m.playbook(ctx, c, "check-cert-expiration.yml", map[string]any{"artifacts_dir": art}); err != nil {
		return time.Time{}, err
	}
	return readCertExpiry(art)
}

// RenewCerts runs renew-certs.yml: `kubeadm certs renew all` + a static-pod restart on every control
// plane (serial for HA), then fetches the freshly-renewed admin kubeconfig and the new earliest
// expiry back to the artifacts dir. The reconciler re-seals SecretKubeconfig with the returned bytes.
func (m *Manager) RenewCerts(ctx context.Context, c *domain.Cluster, renewCutoff time.Time) ([]byte, time.Time, error) {
	art, err := m.prep(c)
	if err != nil {
		return nil, time.Time{}, err
	}
	extra := map[string]any{
		"artifacts_dir": art,
		// now + the renewal window, as Unix epoch seconds: the role renews only a node whose earliest
		// cert still expires before this, so a retry skips nodes an earlier attempt already renewed.
		"cert_renew_cutoff_epoch": renewCutoff.Unix(),
	}
	if err := m.playbook(ctx, c, "renew-certs.yml", extra); err != nil {
		return nil, time.Time{}, err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(art, "admin.conf"))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("ansible: read renewed kubeconfig: %w", err)
	}
	notAfter, err := readCertExpiry(art)
	if err != nil {
		return nil, time.Time{}, err
	}
	return kubeconfig, notAfter, nil
}

// readCertExpiry parses the Unix-epoch expiry the renew_certs role wrote to artifacts_dir/cert-expiry.
func readCertExpiry(art string) (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(art, "cert-expiry"))
	if err != nil {
		return time.Time{}, fmt.Errorf("ansible: read cert expiry: %w", err)
	}
	s := strings.TrimSpace(string(b))
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("ansible: parse cert expiry %q: %w", s, err)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// UpgradeKubernetes runs upgrade.yml: an in-place kubeadm upgrade of every node to target.
// The playbook re-points the pkgs.k8s.io apt repo to target's minor and installs the pinned
// packages, so the same play both bumps and holds the version. Idempotent: a node already at
// target is skipped by the role's version guard.
func (m *Manager) UpgradeKubernetes(ctx context.Context, c *domain.Cluster, target string) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	extra := map[string]any{
		"k8s_version": target,
		"k8s_minor":   minorStr(target),
	}
	return m.playbook(ctx, c, "upgrade.yml", extra)
}

// RemoveControlPlane drains the given control-plane node, removes its etcd member, and deletes
// the node object - all from a surviving control plane (admin_host), so it works even when the
// departing node's VM is about to be replaced. Idempotent.
func (m *Manager) RemoveControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	admin := firstControlPlaneExcept(c, node.VMName)
	if admin == "" {
		return fmt.Errorf("ansible: no surviving control plane to remove %q from", node.VMName)
	}
	extra := map[string]any{"remove_node": node.VMName, "admin_host": admin}
	return m.playbook(ctx, c, "remove-controlplane.yml", extra)
}

// JoinControlPlane joins the given (freshly re-provisioned) node as a control plane. A fresh
// certificate key is minted on a surviving control plane (admin_host) each run - the one printed
// by kubeadm init expires ~2h - so this is safe to retry. Idempotent: skipped if already joined.
func (m *Manager) JoinControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	admin := firstControlPlaneExcept(c, node.VMName)
	if admin == "" {
		return fmt.Errorf("ansible: no surviving control plane to join %q to", node.VMName)
	}
	extra := map[string]any{
		"target_node": node.VMName,
		"admin_host":  admin,
		"k8s_version": c.K8sVersion,
		"k8s_minor":   minorStr(c.K8sVersion),
	}
	// HA: this node was destroyed and recreated (rolling OS upgrade), so it needs the loadbalancer
	// role (keepalived+haproxy) re-provisioned - bootstrap.yml only installs it once, at initial
	// create. Same deterministic values InitControlPlane used, so this node's VRRP config matches
	// its still-running peers.
	if c.HA() {
		extra["ha"] = true
		extra["control_plane_vip"] = c.APIVIP
		extra["vrrp_router_id"] = vridFromVIP(c.APIVIP)
		extra["vrrp_password"] = vrrpPassword(c.ID)
	}
	return m.playbook(ctx, c, "join-controlplane.yml", extra)
}

// BackupControlPlane snapshots etcd and archives /etc/kubernetes and /var/lib/kubelet from the sole
// control plane to the cluster's artifacts dir. Idempotent: if the snapshot already exists (a prior
// run before the VM was replaced), it skips - so the enclosing rolling step can be retried after the
// old VM is gone.
func (m *Manager) BackupControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	art, err := m.prep(c)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(art, "etcd-data.tar.gz")); err == nil {
		m.cfg.Log.Info("ansible", "cluster", c.ID, "line", "control-plane backup already present - skipping")
		return nil
	}
	extra := map[string]any{"target_node": node.VMName, "artifacts_dir": art}
	return m.playbook(ctx, c, "backup-controlplane.yml", extra)
}

// RestoreControlPlane restores the etcd snapshot, /etc/kubernetes, and /var/lib/kubelet onto the
// freshly re-provisioned node and starts the control plane. The node reclaims its old IP via its
// pinned MAC, so the restored certs and etcd member (both keyed on that IP) remain valid.
func (m *Manager) RestoreControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	art, err := m.prep(c)
	if err != nil {
		return err
	}
	extra := map[string]any{
		"target_node":   node.VMName,
		"target_ip":     node.IP,
		"artifacts_dir": art,
		"k8s_version":   c.K8sVersion,
		"k8s_minor":     minorStr(c.K8sVersion),
	}
	if err := m.playbook(ctx, c, "restore-controlplane.yml", extra); err != nil {
		return err
	}
	// The snapshot has been consumed. Discard it so a LATER upgrade of this cluster takes a fresh
	// backup instead of silently reusing this one - reusing it would roll etcd back to this point and
	// drop any nodes (e.g. a worker replaced earlier in the same roll) that joined after it was taken.
	m.discardBackup(art)
	return nil
}

// DiscardControlPlaneBackup removes any control-plane backup archives from the cluster's artifacts
// dir. Called at promotion start so a stale snapshot from an earlier or crashed run can't be reused.
func (m *Manager) DiscardControlPlaneBackup(_ context.Context, c *domain.Cluster) error {
	art, err := m.prep(c)
	if err != nil {
		return err
	}
	m.discardBackup(art)
	return nil
}

// discardBackup deletes the etcd/kube-etc/kubelet archives under art, ignoring "not present".
func (m *Manager) discardBackup(art string) {
	for _, f := range []string{"etcd-data.tar.gz", "kube-etc.tar.gz", "kubelet-data.tar.gz"} {
		if err := os.Remove(filepath.Join(art, f)); err != nil && !os.IsNotExist(err) {
			m.cfg.Log.Warn("ansible", "line", "failed to remove control-plane backup archive", "path", filepath.Join(art, f), "err", err)
		}
	}
}

// firstControlPlaneExcept returns the VM name of the first control-plane node other than exclude
// (the departing/rejoining one), used as the executor for etcd/kubectl management during rolling
// control-plane replacement. Empty if none remains.
func firstControlPlaneExcept(c *domain.Cluster, exclude string) string {
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane && n.VMName != exclude {
			return n.VMName
		}
	}
	return ""
}

// EnsureNodeDisks formats and mounts every worker's extra disks with LVM (see the node_disks role).
// The per-node disk list rides in host_vars, written by prep. Idempotent, so this is safe to run on
// every tick of a cluster that has disks.
//
// A no-op - no ansible-playbook process at all - for a cluster with nothing mountable yet, which is
// every cluster that has no extra disks.
func (m *Manager) EnsureNodeDisks(ctx context.Context, c *domain.Cluster) error {
	if !anyMountableDisks(c) {
		return nil
	}
	if _, err := m.prep(c); err != nil {
		return err
	}
	return m.playbook(ctx, c, "node-disks.yml", map[string]any{})
}

// RemoveNodeDisks tears the given disks down inside the guest (unmount, fstab, vgremove) before
// their volumes are detached. See the ordering note on remove-node-disks.yml.
func (m *Manager) RemoveNodeDisks(ctx context.Context, c *domain.Cluster, disks []domain.NodeDisk) error {
	if len(disks) == 0 {
		return nil
	}
	if _, err := m.prep(c); err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(disks))
	for _, d := range disks {
		out = append(out, map[string]any{
			"vm_name":    d.VMName,
			"name":       d.Name,
			"mount_path": d.MountPath,
		})
	}
	return m.playbook(ctx, c, "remove-node-disks.yml", map[string]any{"remove_disks": out})
}

// longhornNamespace is where the catalog installs the longhorn add-on, and so where its
// node.longhorn.io CRs live.
const longhornNamespace = "longhorn-system"

// EnsureLonghornDisks registers the cluster's extra storage disks with Longhorn (see
// longhorn-disks.yml). A no-op - no ansible-playbook process at all - for the common cluster, whose
// workers carry only the platform's own disk: that one sits at Longhorn's default data path and is
// discovered without help.
func (m *Manager) EnsureLonghornDisks(ctx context.Context, c *domain.Cluster) error {
	nodes := make([]map[string]any, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		disks := domain.StorageDisksFor(c, n.VMName)
		if len(disks) == 0 {
			continue
		}
		patch, err := longhornDiskPatch(disks)
		if err != nil {
			return err
		}
		nodes = append(nodes, map[string]any{"node": n.VMName, "patch": patch})
	}
	if len(nodes) == 0 {
		return nil
	}
	if _, err := m.prep(c); err != nil {
		return err
	}
	return m.playbook(ctx, c, "longhorn-disks.yml", map[string]any{
		"longhorn_nodes":     nodes,
		"longhorn_namespace": longhornNamespace,
	})
}

// longhornDiskPatch builds the JSON merge patch that adds one node's extra disks to its
// node.longhorn.io CR. Built here rather than in Jinja because the repo's playbooks use
// ansible.builtin only (no filter plugin to assemble a nested map with) - and because a patch that
// formats a disk's mount path wrong is worth a unit test.
//
// A MERGE patch is deliberate: it is additive on spec.disks, so Longhorn's own "default-disk-<hash>"
// entry for the platform disk survives untouched, and re-running writes the same values.
func longhornDiskPatch(disks []domain.NodeDisk) (string, error) {
	spec := make(map[string]any, len(disks))
	for _, d := range disks {
		spec[d.LonghornDiskName()] = map[string]any{
			"path": d.MountPath,
			// filesystem (not "block"): these disks carry an ext4/xfs filesystem the node_disks role
			// laid down and mounted, which is exactly what Longhorn's V1 data engine wants.
			"diskType": "filesystem",
			// Explicit rather than defaulted, because this same map is how a disk is drained on the
			// way out - a re-run after a cancelled removal has to put it back into service.
			"allowScheduling":   true,
			"evictionRequested": false,
			// The whole disk is the user's: they asked for it and are charged quota for it. Longhorn's
			// storageMinimalAvailablePercentage (set in the catalog) is what keeps it from filling to
			// the last byte.
			"storageReserved": 0,
			"tags":            []string{},
		}
	}
	b, err := json.Marshal(map[string]any{"spec": map[string]any{"disks": spec}})
	if err != nil {
		return "", fmt.Errorf("build longhorn disk patch: %w", err)
	}
	return string(b), nil
}

// EvictLonghornDisks drains Longhorn off the given disks and drops them from their nodes' CRs,
// before anything unmounts them (see longhorn-evict.yml). A no-op for disks Longhorn never knew
// about - an ordinary mounted filesystem, or a cluster with no longhorn add-on at all.
func (m *Manager) EvictLonghornDisks(ctx context.Context, c *domain.Cluster, disks []domain.NodeDisk) error {
	out := make([]map[string]any, 0, len(disks))
	for _, d := range disks {
		// Deliberately NOT NeedsLonghornRegistration: by the time a disk is being removed its phase
		// is "removing", so the predicate that decides what to REGISTER cannot decide what to
		// UNregister. What matters here is only whether Longhorn was ever told about it.
		if d.FeedsStoragePool() && !d.IsPlatformStorage() {
			out = append(out, map[string]any{"node": d.VMName, "disk": d.LonghornDiskName()})
		}
	}
	if len(out) == 0 {
		return nil
	}
	if _, err := m.prep(c); err != nil {
		return err
	}
	return m.playbook(ctx, c, "longhorn-evict.yml", map[string]any{
		"evict_disks":        out,
		"longhorn_namespace": longhornNamespace,
	})
}

// prep ensures the per-cluster workspace + artifacts dir and (re)writes the inventory.
func (m *Manager) prep(c *domain.Cluster) (string, error) {
	dir := filepath.Join(m.cfg.WorkDir, c.ID)
	art := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(art, 0o700); err != nil {
		return "", err
	}
	if err := m.writeInventory(c, dir); err != nil {
		return "", err
	}
	if err := m.writeHostVars(c, dir); err != nil {
		return "", err
	}
	return art, nil
}

// writeHostVars writes each worker's extra-disk list to host_vars/<node>.yml, which Ansible loads
// automatically from the inventory's own directory.
//
// host_vars rather than an inline inventory var because the value is a LIST OF OBJECTS and the
// inventory is INI, which can only carry scalars. Rather than flatten it into some ad-hoc string the
// role would have to parse back, each node gets a small YAML file - which is also the shape the
// node_disks role's contract wants ("node_disks is a per-host var").
//
// The directory is rewritten from scratch on every prep: these files are derived state, and a stale
// one (a disk the user has since removed) would have the role re-mount something the platform is
// busy tearing down.
func (m *Manager) writeHostVars(c *domain.Cluster, dir string) error {
	hv := filepath.Join(dir, "host_vars")
	if err := os.RemoveAll(hv); err != nil {
		return err
	}
	if !anyMountableDisks(c) {
		return nil // nothing to say; leave the directory absent entirely
	}
	if err := os.MkdirAll(hv, 0o700); err != nil {
		return err
	}
	for _, n := range c.Nodes {
		if n.Role != domain.RoleWorker {
			continue
		}
		disks := mountableDisks(c, n.VMName)
		if len(disks) == 0 {
			continue
		}
		vars := map[string]any{"node_disks": disks}
		b, err := json.MarshalIndent(vars, "", "  ")
		if err != nil {
			return err
		}
		// JSON is valid YAML, so this needs no YAML encoder - and it round-trips the structure
		// exactly, with no quoting subtleties for a mount path.
		if err := os.WriteFile(filepath.Join(hv, n.VMName+".yml"), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// mountableDisks is one node's extra disks that should currently be formatted and mounted, in stable
// order.
//
// Two exclusions, both load-bearing:
//   - a disk being REMOVED is left out, or the role would faithfully re-mount the disk the platform
//     is in the middle of tearing down, and the two would fight every tick.
//   - a disk with no DeviceID is left out: the provisioner has not reported its identity yet (it may
//     not exist), and without one there is no safe way to choose a device to format. It appears here
//     on a later tick, once observed.
func mountableDisks(c *domain.Cluster, vmName string) []map[string]any {
	var out []map[string]any
	for _, d := range domain.DisksFor(c, vmName) {
		if d.Phase == domain.DiskPhaseRemoving || d.DeviceID == "" {
			continue
		}
		out = append(out, map[string]any{
			"name":       d.Name,
			"device_id":  d.DeviceID,
			"mount_path": d.MountPath,
			"fs_type":    d.FSType,
		})
	}
	return out
}

// anyMountableDisks reports whether any node has a disk to format/mount right now.
func anyMountableDisks(c *domain.Cluster) bool {
	for _, d := range c.NodeDisks {
		if d.Phase != domain.DiskPhaseRemoving && d.DeviceID != "" {
			return true
		}
	}
	return false
}

func (m *Manager) writeInventory(c *domain.Cluster, dir string) error {
	var b strings.Builder
	b.WriteString("[control_plane]\n")
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			fmt.Fprintf(&b, "%s ansible_host=%s\n", n.VMName, n.IP)
		}
	}
	// Each worker carries its node pool as a host var, which the worker role turns into the kubelet's
	// --node-labels at registration (see domain.PoolLabel). Control planes belong to no pool and get
	// no such var.
	b.WriteString("\n[workers]\n")
	for _, n := range c.Nodes {
		if n.Role == domain.RoleWorker {
			fmt.Fprintf(&b, "%s ansible_host=%s nodepool=%s\n", n.VMName, n.IP, n.Pool)
		}
	}
	fmt.Fprintf(&b, "\n[all:vars]\nansible_user=%s\nansible_ssh_private_key_file=%s\nansible_python_interpreter=/usr/bin/python3\n",
		m.cfg.SSHUser, m.cfg.SSHPrivateKeyFile)
	return os.WriteFile(filepath.Join(dir, "inventory.ini"), []byte(b.String()), 0o644)
}

func (m *Manager) playbook(ctx context.Context, c *domain.Cluster, name string, extra map[string]any) error {
	dir := filepath.Join(m.cfg.WorkDir, c.ID)
	varsPath := filepath.Join(dir, "extravars-"+strings.TrimSuffix(name, ".yml")+".json")
	// A remote KVM host puts the VMs behind the hypervisor, so every play must hop through it. This
	// rides in the extra-vars (highest precedence) rather than the inventory's [all:vars] purely to
	// dodge INI quoting: the value is itself a quoted ssh command line. Absent when the host is local.
	if m.cfg.SSHCommonArgs != "" {
		extra["ansible_ssh_common_args"] = m.cfg.SSHCommonArgs
	}
	// etcd backend tuning rides on EVERY play, like the ssh args above, rather than being threaded
	// into the three call sites that need it (bootstrap, join-controlplane, etcd-defrag). The roles
	// that don't read these vars ignore them, and the ones that do can never disagree about the
	// values - which matters, because a member tuned differently from its peers is the one that
	// still hits the 2GiB cliff. Absent when unset: the roles carry the same defaults.
	if m.cfg.EtcdQuotaBytes > 0 {
		extra["etcd_quota_backend_bytes"] = m.cfg.EtcdQuotaBytes
	}
	if m.cfg.EtcdCompactionRetention != "" {
		extra["etcd_auto_compaction_retention"] = m.cfg.EtcdCompactionRetention
	}
	// Registry trust + pull-through mirrors ride on EVERY play, for the same reason the etcd tuning
	// does and one more: the `common` role (which carries registry_trust) runs on the bootstrap,
	// join, rejoin and restore paths, and a node that missed the configuration on any one of them
	// would silently pull from the internet forever. Threading it into those call sites individually
	// is exactly the drift this avoids. Absent when no registry is configured, and the role is a
	// no-op then. See internal/config/ansible/registry.go.
	for k, v := range m.registryVars() {
		extra[k] = v
	}
	b, err := json.MarshalIndent(extra, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(varsPath, b, 0o644); err != nil {
		return err
	}
	env := append(os.Environ(),
		"ANSIBLE_CONFIG="+filepath.Join(m.cfg.PlaybookDir, "ansible.cfg"),
		"ANSIBLE_HOST_KEY_CHECKING=False",
	)
	emit := func(line string) {
		m.cfg.Log.Info("ansible", "cluster", c.ID, "line", line)
		if m.cfg.Events != nil {
			m.cfg.Events.Emit(events.Event{ClusterID: c.ID, Level: "info", Source: "ansible", Message: line})
		}
	}
	playbookPath := filepath.Join(m.cfg.PlaybookDir, "playbooks", name)
	inv := filepath.Join(dir, "inventory.ini")
	return procstream.Run(ctx, m.cfg.PlaybookDir, env, emit, m.cfg.Bin, "-i", inv, playbookPath, "-e", "@"+varsPath)
}

// vridFromVIP derives a keepalived VRRP router id (1-255) from the VIP's last octet. VIPs
// are unique per cluster, so the router ids are too - which keepalived requires among VRRP
// instances sharing an L2 segment (here, the libvirt bridge).
func vridFromVIP(vip string) int {
	last := vip[strings.LastIndex(vip, ".")+1:]
	n, err := strconv.Atoi(last)
	if err != nil || n < 1 {
		return 1
	}
	if n > 255 {
		return 255
	}
	return n
}

// vrrpPassword is a short, per-cluster VRRP auth secret (keepalived truncates to 8 bytes).
// Not a security control - just keeps two clusters' VRRP chatter from being mistaken for
// each other on the shared bridge.
func vrrpPassword(clusterID string) string {
	if len(clusterID) >= 8 {
		return clusterID[:8]
	}
	return clusterID
}

// minorStr reduces "1.36.2" to "1.36" (used for the pkgs.k8s.io repo path).
func minorStr(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}
