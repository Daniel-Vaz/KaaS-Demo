// Package config is the configuration-management seam: turn provisioned VMs into a
// kubeadm cluster.
//
// The real implementation invokes Ansible via ansible-runner with a dynamic inventory
// generated from the cluster's nodes. Fake simulates it so the control loop
// can run without SSH or real VMs.
package config

import (
	"context"
	"fmt"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Manager forms and configures the Kubernetes cluster over the provisioned nodes.
// All methods must be idempotent (kubeadm steps guarded by readiness checks).
type Manager interface {
	// InitControlPlane runs `kubeadm init` on the control-plane node and returns the
	// admin kubeconfig and a worker join token.
	InitControlPlane(ctx context.Context, c *domain.Cluster) (kubeconfig []byte, joinToken string, err error)
	// EnsureViewerKubeconfig applies read-only RBAC to the cluster (a "kaas-viewer" ServiceAccount
	// bound to the built-in `view` ClusterRole plus a small cluster-scoped read role - everything
	// except secrets) and returns a kubeconfig bound to it. Handed to read-role group-mates so they
	// get a genuinely read-only credential, never cluster-admin. Idempotent (kubectl apply); runs
	// after the API server is up (from PhaseWorkersReady).
	EnsureViewerKubeconfig(ctx context.Context, c *domain.Cluster) (kubeconfig []byte, err error)
	// JoinWorkers joins every worker in c.Nodes not already in the cluster. Idempotent:
	// already-joined nodes are skipped, so it also serves scale-up. It mints a
	// fresh join token on the control plane each run, so scaling works long after create
	// (the bootstrap token expires).
	JoinWorkers(ctx context.Context, c *domain.Cluster) error
	// RemoveWorkers cordons/drains and deletes the given worker nodes from the cluster
	// (scale-down), so kubeadm forgets them before their VMs are destroyed. Idempotent.
	RemoveWorkers(ctx context.Context, c *domain.Cluster, workers []domain.Node) error

	// EnsureNodeDisks formats and mounts every worker's extra disks (domain.NodeDisk) with LVM:
	// device -> PV -> VG -> LV -> filesystem -> mount, persisted in fstab. Runs AFTER the
	// provisioner has attached the volumes, and only for disks whose DeviceID the provisioner has
	// reported - without one there is no safe way to tell which device to format. Idempotent
	// (every step is guarded), so it re-runs on each tick of a cluster that has disks.
	EnsureNodeDisks(ctx context.Context, c *domain.Cluster) error
	// RemoveNodeDisks unmounts the given disks, drops their fstab entries and destroys their volume
	// groups, IN THE GUEST - before the provisioner detaches and destroys the underlying volumes.
	// That order is the contract: unmounting after the device has gone leaves the node with a stale
	// mount over a dead device. Idempotent: every step tolerates the object already being gone.
	RemoveNodeDisks(ctx context.Context, c *domain.Cluster, disks []domain.NodeDisk) error

	// EnsureLonghornDisks registers each node's EXTRA storage disks with the in-cluster Longhorn
	// (patching its node.longhorn.io CR), so they count as pool capacity. Only the additional disks
	// a user attaches need this: the platform's per-worker disk is mounted at Longhorn's own default
	// data path and is discovered without help - registering it twice is an error, not a no-op.
	// Idempotent (a merge patch, additive per disk key), run from reconcileStorageWiring once the
	// longhorn add-on is installed.
	EnsureLonghornDisks(ctx context.Context, c *domain.Cluster) error
	// EvictLonghornDisks asks Longhorn to move every replica off the given disks and then drops them
	// from their nodes' CRs - BEFORE RemoveNodeDisks unmounts them. Same ordering contract as
	// draining a worker before destroying its VM, and for a sharper reason: unmounting a registered
	// disk under Longhorn degrades every volume with a replica on it. Idempotent, and deliberately
	// gives up after a bounded wait rather than wedging the removal forever (see longhorn-evict.yml).
	EvictLonghornDisks(ctx context.Context, c *domain.Cluster, disks []domain.NodeDisk) error
	// InstallCNI installs the cluster's chosen CNI.
	InstallCNI(ctx context.Context, c *domain.Cluster) error
	// EnsureCNIMetrics re-configures the CNI to publish Prometheus ServiceMonitors (a helm upgrade
	// that turns on the CNI's metrics + ServiceMonitor). Run only AFTER the monitoring stack is up,
	// because a ServiceMonitor needs the Prometheus Operator's CRD - which doesn't exist yet when the
	// CNI is first installed during bootstrap. Idempotent (helm upgrade --install).
	EnsureCNIMetrics(ctx context.Context, c *domain.Cluster) error
	// EnsureControlPlaneMetrics makes etcd/kube-scheduler/kube-controller-manager/kube-proxy
	// reachable by Prometheus: kubeadm's defaults bind the scheduler/controller-manager metrics port
	// and kube-proxy's metrics server to loopback only, and etcd exposes no unauthenticated metrics
	// listener at all - kube-prometheus-stack's built-in ServiceMonitors for these components (all
	// enabled by default) silently fail to scrape until this runs. Unlike EnsureCNIMetrics this has
	// no CRD dependency (it never creates a ServiceMonitor itself - kube-prometheus-stack already
	// ships one for each), so it only needs to run once monitoring is actually installed as a matter
	// of not widening the control plane's exposed surface on a cluster that never asked for it.
	// Idempotent (manifest edits + a ConfigMap merge patch).
	EnsureControlPlaneMetrics(ctx context.Context, c *domain.Cluster) error
	// EnsureDefaultGateway applies the cluster's default north-south ingress: a MetalLB IPAddressPool
	// (a single /32 built from c.LoadBalancerIP) + L2Advertisement, and the Envoy GatewayClass + a
	// default Gateway bound to it and pinned to that address. Run only once the metallb + envoy-gateway
	// add-ons are installed (the reconciler gates on that) and c.LoadBalancerIP is set. Idempotent
	// (`kubectl apply`), so it re-runs cleanly on reconcile retries.
	EnsureDefaultGateway(ctx context.Context, c *domain.Cluster) error

	// EnsureExternalSecrets applies the in-cluster half of the Vault integration: a ClusterSecretStore
	// pointing the External Secrets Operator at the central Vault over the cluster's own JWT auth role
	// (see the external_secrets ansible role). Run only once the external-secrets add-on is installed
	// (the reconciler gates on that). Idempotent (`kubectl apply`), so it re-runs cleanly on retries.
	EnsureExternalSecrets(ctx context.Context, c *domain.Cluster) error
	// EnsureRegistryPullSecret applies the in-cluster half of the image-registry integration: a
	// dockerconfigjson Secret holding the cluster's own push/pull robot, so workloads can pull from
	// (and CI can push to) the cluster's private project without anyone minting a credential by hand.
	// Idempotent (`kubectl apply` of a generated Secret), so it re-runs cleanly on retries.
	//
	// It carries the credential as plain strings rather than a registry type on purpose: this seam is
	// about running playbooks, and the only thing it needs to know is what to write.
	EnsureRegistryPullSecret(ctx context.Context, c *domain.Cluster, username, secret string) error
	// ClusterOIDC returns the cluster's service-account token ISSUER and its PUBLIC signing keys
	// (PEM-encoded, from the cluster's /openid/v1/jwks), so the platform can configure Vault to
	// validate the External Secrets Operator's ServiceAccount token OFFLINE - no Vault→cluster
	// reachability. Read-only. The Fake returns empty, which makes the Vault wiring skip the JWT auth
	// role and just provision the path + policies (so `make up-fake` needs no cluster to read keys from).
	ClusterOIDC(ctx context.Context, c *domain.Cluster) (issuer string, publicKeysPEM []string, err error)

	// CertExpiry reports the earliest expiry across the cluster's kubeadm-managed control-plane
	// certificates (via `kubeadm certs check-expiration` on a control plane). Read-only: it observes,
	// it does not renew. Used to stamp domain.Cluster.CertNotAfter on clusters that predate automatic
	// rotation and to feed the certificate health check. Idempotent.
	CertExpiry(ctx context.Context, c *domain.Cluster) (time.Time, error)
	// RenewCerts renews the cluster's kubeadm-managed control-plane certificates on every control-plane
	// node still within the renewal window (`kubeadm certs renew all` - each node holds the full PKI
	// under stacked etcd), restarts the control-plane static pods so they pick up the new certs, and
	// returns a freshly-fetched admin kubeconfig (whose embedded client cert has likewise been renewed)
	// plus the new earliest expiry. renewCutoff is now + the renewal window: a node whose earliest cert
	// already expires after it was renewed by an earlier attempt and is skipped, which is what makes a
	// retried step (HA renews one node at a time, and the reconcile job can time out mid-run) resume
	// instead of re-bouncing every control plane. The caller re-seals SecretKubeconfig with the returned
	// bytes, because every worker-side seam reads it. It does NOT touch the CA (10-year, renewing it is
	// disruptive) or kubelet certs (kubeadm enables their auto-rotation).
	RenewCerts(ctx context.Context, c *domain.Cluster, renewCutoff time.Time) (kubeconfig []byte, notAfter time.Time, err error)

	// EtcdStatus reports the cluster's etcd backend store: the worst member's physical size and
	// logically-used size, its configured quota, any armed alarms, and how many members answered
	// (`etcdctl endpoint status --cluster` + `alarm list`, from the etcd pod on a control plane).
	// Read-only: it observes, it does not defragment - the same observe/act split CertExpiry and
	// RenewCerts have, and for the same reason (the cheap always-safe read stays out of the
	// disruptive step). The member count is load-bearing, not decoration: it is how the caller knows
	// a member is unreachable, which forbids defragmenting. Idempotent.
	EtcdStatus(ctx context.Context, c *domain.Cluster) (domain.EtcdStatus, error)
	// DefragEtcd defragments every control plane's etcd, ONE MEMBER AT A TIME, and disarms any
	// NOSPACE alarm afterwards. Defragmentation is stop-the-world for the member it runs on - it
	// takes the backend file's lock and serves nothing until it finishes - so the playbook refuses
	// to start unless every member is healthy, moves raft leadership off a member before
	// defragmenting it, and gates on the member being healthy again before advancing. On a SINGLE
	// control plane this is a brief API outage, the same trade the etcd restore path makes.
	//
	// It also converges the backend tuning (quota + auto-compaction) onto members that predate the
	// controlplane_etcd role, since it is already taking those members' outage under a quorum gate.
	//
	// Returns the post-defragmentation status so the caller can stamp the reclaimed size without a
	// second observation. Idempotent, and resumable: a member already below the fragmentation
	// threshold is skipped, so a run killed by the job timeout picks up where it stopped instead of
	// re-bouncing members it already did.
	DefragEtcd(ctx context.Context, c *domain.Cluster, minRatio float64) (domain.EtcdStatus, error)

	// UpgradeKubernetes performs an in-place kubeadm upgrade of every node to target (a
	// "major.minor.patch" version): `kubeadm upgrade apply` on the first control plane, then
	// `kubeadm upgrade node` on the rest, draining and bumping kubelet/kubectl per node.
	// Idempotent - a node already at target is skipped.
	UpgradeKubernetes(ctx context.Context, c *domain.Cluster, target string) error
	// RemoveControlPlane drains the given control-plane node, removes its etcd member, and
	// deletes it from the cluster (run from the first control plane), so the cluster forgets it
	// before its VM is replaced during a rolling OS upgrade. Idempotent.
	RemoveControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error
	// JoinControlPlane joins the given (freshly re-provisioned) node as a control plane, minting
	// a fresh certificate key on the first control plane. Idempotent: skipped if already joined.
	JoinControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error

	// BackupControlPlane snapshots etcd and archives /etc/kubernetes from the given (sole) control
	// plane to the controller, so a single-node control plane can be rebuilt onto a new OS image
	// without losing state. Idempotent: skipped if a backup already exists.
	BackupControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error
	// RestoreControlPlane restores the etcd snapshot + /etc/kubernetes captured by
	// BackupControlPlane onto the given freshly re-provisioned node (which reclaims the old node's
	// IP via its pinned MAC, so the restored certs/etcd stay valid) and starts the control plane.
	// On success it discards the backup archives, so a later upgrade cannot silently reuse this
	// (now-consumed) snapshot and roll etcd back over nodes that joined after it was taken.
	RestoreControlPlane(ctx context.Context, c *domain.Cluster, node domain.Node) error
	// DiscardControlPlaneBackup deletes any control-plane backup archives left in the cluster's
	// artifacts dir. The reconciler calls this at the start of a promotion so a stale snapshot from
	// an earlier (possibly crashed) run can't be reused. Idempotent - a no-op when none exist.
	DiscardControlPlaneBackup(ctx context.Context, c *domain.Cluster) error

	// SnapshotEtcd takes a PERIODIC control-plane backup: an ONLINE `etcdctl snapshot save` plus the
	// cluster PKI and kubelet state, returned as one opaque archive for the caller to seal and store.
	//
	// It is deliberately NOT BackupControlPlane. That one takes a raw copy of /var/lib/etcd, which is
	// only consistent with etcd stopped - so it quiesces the control plane, which is correct there
	// (the node is destroyed immediately afterwards) and unusable on a cadence. This one stops
	// nothing and is safe to run against every healthy cluster on the platform, which is the whole
	// reason a periodic backup is possible at all.
	//
	// The returned metadata carries the etcd revision and hash read back by a VERIFICATION step: a
	// snapshot that could not be read back is not stored, because a corrupt backup is worse than no
	// backup - it satisfies retention, silences the staleness check, and fails at the only moment it
	// is ever used. Read-only and idempotent; every call produces a fresh snapshot.
	SnapshotEtcd(ctx context.Context, c *domain.Cluster) (domain.EtcdSnapshot, []byte, error)
	// RestoreEtcdSnapshot rebuilds a SOLE control plane on the given freshly re-provisioned node from
	// the archive returned by SnapshotEtcd. The node reclaims its old IP via its pinned MAC, so the
	// restored certs and the member identity remain valid.
	//
	// This is the platform's only LOSSY operation: every API object written after the snapshot was
	// taken is gone. It is reserved for the fault nothing else can reach - a single-control-plane
	// cluster with no live node to copy state from - and is gated on its own flag
	// (KAAS_REPAIR_RESTORE) for that reason. Idempotent: guarded by a marker on the node.
	RestoreEtcdSnapshot(ctx context.Context, c *domain.Cluster, node domain.Node, archive []byte) error

	// RestartKubelet restarts the container runtime and kubelet on ONE node - the cheapest rung of
	// automatic node repair, and the fix for most NotReady nodes. It runs against the node itself
	// rather than from a control plane, so an unreachable node fails here; that failure is the
	// signal, not an error to paper over, and the reconciler escalates to replacement. Idempotent.
	RestartKubelet(ctx context.Context, c *domain.Cluster, node domain.Node) error
}

// Fake returns a plausible-looking kubeconfig without touching any VM.
type Fake struct{}

func NewFake() *Fake { return &Fake{} }

func (Fake) InitControlPlane(_ context.Context, c *domain.Cluster) ([]byte, string, error) {
	var cpIP string
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			cpIP = n.IP
		}
	}
	kubeconfig := fmt.Appendf(nil,
		"apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://%s:6443\n  name: %s\n# NOTE: fake kubeconfig generated by the demo control plane\n",
		cpIP, c.Name)
	return kubeconfig, "fake.token.0123456789abcdef", nil
}

func (Fake) EnsureViewerKubeconfig(_ context.Context, c *domain.Cluster) ([]byte, error) {
	var cpIP string
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			cpIP = n.IP
		}
	}
	kubeconfig := fmt.Appendf(nil,
		"apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://%s:6443\n  name: %s\nusers:\n- name: kaas-viewer\n# NOTE: fake READ-ONLY viewer kubeconfig generated by the demo control plane\n",
		cpIP, c.Name)
	return kubeconfig, nil
}

func (Fake) JoinWorkers(_ context.Context, _ *domain.Cluster) error { return nil }
func (Fake) EnsureNodeDisks(_ context.Context, _ *domain.Cluster) error {
	return nil
}
func (Fake) RemoveNodeDisks(_ context.Context, _ *domain.Cluster, _ []domain.NodeDisk) error {
	return nil
}
func (Fake) EnsureLonghornDisks(_ context.Context, _ *domain.Cluster) error { return nil }
func (Fake) EvictLonghornDisks(_ context.Context, _ *domain.Cluster, _ []domain.NodeDisk) error {
	return nil
}
func (Fake) RemoveWorkers(_ context.Context, _ *domain.Cluster, _ []domain.Node) error {
	return nil
}
func (Fake) InstallCNI(_ context.Context, _ *domain.Cluster) error                { return nil }
func (Fake) EnsureCNIMetrics(_ context.Context, _ *domain.Cluster) error          { return nil }
func (Fake) EnsureControlPlaneMetrics(_ context.Context, _ *domain.Cluster) error { return nil }
func (Fake) EnsureDefaultGateway(_ context.Context, _ *domain.Cluster) error      { return nil }
func (Fake) EnsureExternalSecrets(_ context.Context, _ *domain.Cluster) error     { return nil }

func (Fake) EnsureRegistryPullSecret(_ context.Context, _ *domain.Cluster, _, _ string) error {
	return nil
}

func (Fake) ClusterOIDC(_ context.Context, _ *domain.Cluster) (string, []string, error) {
	return "", nil, nil
}
func (Fake) CertExpiry(_ context.Context, _ *domain.Cluster) (time.Time, error) {
	// A plausible ~1-year-out expiry, so the health check renders and rotation stays dormant in
	// fake mode (the demo never reaches the renewal window).
	return time.Now().AddDate(1, 0, 0), nil
}

func (Fake) RenewCerts(_ context.Context, c *domain.Cluster, _ time.Time) ([]byte, time.Time, error) {
	kubeconfig, _, err := Fake{}.InitControlPlane(context.Background(), c)
	return kubeconfig, time.Now().AddDate(1, 0, 0), err
}

// fakeEtcdFragmentationPerMinute is how fast the fake's backend store fragments, chosen so a demo
// cluster crosses the default 45% threshold about twenty minutes after it is created (or after its
// last defragmentation). Fast enough that `make up-fake` can actually show the DefragmentingEtcd
// phase happening; slow enough that it isn't the only thing on the events timeline.
const fakeEtcdFragmentationPerMinute = 0.0225

// EtcdStatus synthesizes a plausible, DRIFTING backend store so the whole maintenance path - the
// observation cadence, the threshold, the phase, the events - is reachable without a real cluster.
//
// The drift is derived from the cluster's own stored state (time since the last defragmentation, or
// since creation) rather than from a counter in this process. That is what makes it a genuine
// saw-tooth across restarts and across replicas: fragmentation climbs, the reconciler defragments,
// DefraggedAt moves, and the next observation reads a freshly compacted store. A fake holding the
// drift in memory would instead re-report the same fragmented numbers forever and defragment on
// every tick.
func (Fake) EtcdStatus(_ context.Context, c *domain.Cluster) (domain.EtcdStatus, error) {
	since := c.CreatedAt
	if c.Etcd != nil && c.Etcd.DefraggedAt != nil {
		since = *c.Etcd.DefraggedAt
	}
	ratio := min(0.60, time.Since(since).Minutes()*fakeEtcdFragmentationPerMinute)
	if ratio < 0 {
		ratio = 0
	}
	// A plausible keyspace: a base plus a little per node, comfortably over the 100MiB floor so the
	// fragmentation threshold is the thing being demonstrated rather than the floor.
	inUse := int64(96<<20) + int64(len(c.Nodes))*(4<<20)
	return domain.EtcdStatus{
		DBBytes:      int64(float64(inUse) / (1 - ratio)),
		DBInUseBytes: inUse,
		QuotaBytes:   domain.EtcdDefaultQuotaBytes * 4, // as if the controlplane_etcd role had run
		Members:      c.ControlPlanes,
		ObservedAt:   time.Now().UTC(),
		DefraggedAt:  fakeDefraggedAt(c),
	}, nil
}

// DefragEtcd reclaims the fake's fragmentation: the store comes back compacted and stamped, which is
// what makes the next EtcdStatus start the drift over.
func (Fake) DefragEtcd(_ context.Context, c *domain.Cluster, _ float64) (domain.EtcdStatus, error) {
	inUse := int64(96<<20) + int64(len(c.Nodes))*(4<<20)
	now := time.Now().UTC()
	return domain.EtcdStatus{
		DBBytes:      inUse,
		DBInUseBytes: inUse,
		QuotaBytes:   domain.EtcdDefaultQuotaBytes * 4,
		Members:      c.ControlPlanes,
		ObservedAt:   now,
		DefraggedAt:  &now,
	}, nil
}

func fakeDefraggedAt(c *domain.Cluster) *time.Time {
	if c.Etcd == nil {
		return nil
	}
	return c.Etcd.DefraggedAt
}

func (Fake) UpgradeKubernetes(_ context.Context, _ *domain.Cluster, _ string) error {
	return nil
}
func (Fake) RemoveControlPlane(_ context.Context, _ *domain.Cluster, _ domain.Node) error {
	return nil
}
func (Fake) JoinControlPlane(_ context.Context, _ *domain.Cluster, _ domain.Node) error {
	return nil
}
func (Fake) BackupControlPlane(_ context.Context, _ *domain.Cluster, _ domain.Node) error {
	return nil
}
func (Fake) RestoreControlPlane(_ context.Context, _ *domain.Cluster, _ domain.Node) error {
	return nil
}
func (Fake) DiscardControlPlaneBackup(_ context.Context, _ *domain.Cluster) error {
	return nil
}

// SnapshotEtcd synthesizes a plausible backup so the whole snapshot cadence - the due-scan, the
// phase, retention, the staleness health check and the portal's backup list - is demoable under
// `make up-fake`. The payload is a small deterministic blob rather than a real archive: it is opaque
// to everything above this seam, and the fake restore below never unpacks it.
func (Fake) SnapshotEtcd(_ context.Context, c *domain.Cluster) (domain.EtcdSnapshot, []byte, error) {
	now := time.Now().UTC()
	// A revision that grows with the cluster's age, so successive fake snapshots differ the way real
	// ones do and "how much would a restore roll back" reads sensibly in the portal.
	rev := int64(1000) + int64(time.Since(c.CreatedAt).Seconds())
	payload := fmt.Appendf(nil, "kaas-fake-etcd-snapshot cluster=%s revision=%d taken=%s\n", c.ID, rev, now.Format(time.RFC3339))
	var node string
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			node = n.VMName
			break
		}
	}
	return domain.EtcdSnapshot{
		TakenAt:    now,
		Revision:   rev,
		Hash:       uint32(rev),
		SizeBytes:  int64(len(payload)),
		K8sVersion: c.K8sVersion,
		NodeName:   node,
	}, payload, nil
}

func (Fake) RestoreEtcdSnapshot(_ context.Context, _ *domain.Cluster, _ domain.Node, _ []byte) error {
	return nil
}

func (Fake) RestartKubelet(_ context.Context, _ *domain.Cluster, _ domain.Node) error {
	return nil
}
