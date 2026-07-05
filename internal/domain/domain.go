// Package domain holds the core types and the cluster lifecycle state machine.
// It has no dependencies on other internal packages - everything points inward to it.
package domain

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Phase is the cluster lifecycle state. See docs/architecture.md for the diagram.
type Phase string

const (
	PhasePending           Phase = "Pending"
	PhaseProvisioningInfra Phase = "ProvisioningInfra"
	PhaseInfraReady        Phase = "InfraReady"
	PhaseControlPlaneReady Phase = "ControlPlaneReady"
	PhaseWorkersReady      Phase = "WorkersReady"
	PhaseInstallingAddons  Phase = "InstallingAddons"
	PhaseReady             Phase = "Ready"
	PhaseUpdating          Phase = "Updating"
	PhaseUpgrading         Phase = "Upgrading"
	PhaseRenewingCerts     Phase = "RenewingCerts"
	PhaseDefragmentingEtcd Phase = "DefragmentingEtcd"
	PhaseSnapshottingEtcd  Phase = "SnapshottingEtcd"
	PhaseRepairing         Phase = "Repairing"
	PhaseDeleting          Phase = "Deleting"
	PhaseDeleted           Phase = "Deleted"
	PhaseFailed            Phase = "Failed"
)

// Terminal reports whether the phase is an end state the reconciler leaves alone.
func (p Phase) Terminal() bool { return p == PhaseDeleted || p == PhaseFailed }

// Role is a node's role in the cluster.
type Role string

const (
	RoleControlPlane Role = "control-plane"
	RoleWorker       Role = "worker"
)

// SecretKind identifies a piece of sensitive material stored (encrypted) per cluster.
type SecretKind string

const (
	SecretKubeconfig SecretKind = "kubeconfig"
	// SecretKubeconfigViewer is a read-only kubeconfig bound to an in-cluster viewer ServiceAccount
	// (RBAC: read everything except secrets). Handed to read-role group-mates for download and the
	// shell, so they never hold the cluster-admin credential (SecretKubeconfig).
	SecretKubeconfigViewer SecretKind = "kubeconfig_viewer"
	SecretJoinToken        SecretKind = "join_token"
	SecretSSHKey           SecretKind = "ssh_key"
)

// GroupRole is a user's role within a single group - a coarse read/write RBAC over that group's
// members' clusters (see authorizeClusterWrite in internal/app). It governs access to OTHER members'
// clusters only: a user always retains full control of clusters they own, whatever their role.
// Read (the default) may only view group-mates' clusters; Write may also manage them (scale,
// upgrade, delete, kubeconfig, shell). Admin-assigned; a user can't change their own role. The
// role is scoped to one membership - a user in several groups can be Read in one and Write in
// another - and moot for admins (who have full access to everything regardless).
type GroupRole string

const (
	GroupRoleRead  GroupRole = "read"  // view group-mates' clusters only (default)
	GroupRoleWrite GroupRole = "write" // manage group-mates' clusters, the same as their owner
)

// Valid reports whether r is a known role. Used to validate admin role assignments.
func (r GroupRole) Valid() bool { return r == GroupRoleRead || r == GroupRoleWrite }

// KubeGroupWriters and KubeGroupReaders are the two synthetic Kubernetes groups a downloaded,
// per-user kubeconfig's client certificate carries in its Subject Organization (O=). They are the
// bridge between the platform's read/write model and in-cluster RBAC: the cluster carries one static
// ClusterRoleBinding per group (kaas:writers → cluster-admin, kaas:readers → view + a small
// cluster-scoped read role, applied by the viewer_kubeconfig ansible role), and the platform stamps
// the group matching the actor's RESOLVED access (accessTo) into the cert at download time. Encoding
// the resolved role - not the raw directory groups - is what keeps the cluster bindings trivial and
// untenanted while making cluster authorization identical to the portal's by construction. The
// `kaas:` prefix keeps them clear of `system:` (the kube-apiserver-client signer refuses to sign a
// CSR requesting system:masters, and cluster-admin here comes from the RBAC binding, not that group).
const (
	KubeGroupWriters = "kaas:writers"
	KubeGroupReaders = "kaas:readers"
)

// KubeGroupForRole maps a resolved group role to the Kubernetes group a minted client cert carries.
// Write → kaas:writers (cluster-admin), anything else → kaas:readers (read-only).
func KubeGroupForRole(r GroupRole) string {
	if r == GroupRoleWrite {
		return KubeGroupWriters
	}
	return KubeGroupReaders
}

// GroupMembership is one edge of a user's multi-group membership: the group they belong to and their
// coarse read/write role within it. A user carries a set of these (User.Memberships); the role is
// per-group, so a user can be Read in one group and Write in another (see accessTo in internal/app).
type GroupMembership struct {
	GroupID string    `json:"group_id"`
	Role    GroupRole `json:"role"`
}

// User is a local tenant account. Clusters are owned by a user; a normal user sees and manages
// only their own, while an admin manages every user and sees every cluster. Quota is per-user,
// carved from the fixed platform total under a conserved-pool invariant (see internal/quota):
// the sum of all users' quota can never exceed the host budget, so real VM usage can't
// oversubscribe the host. Self-registered users start at zero quota until an admin grants some.
//
// Authn is deliberately simple (see CLAUDE.md fidelity stance): local accounts with bcrypt
// password hashes and signed-cookie sessions. Production would use an IdP/OIDC, a real session
// store, and a KMS-managed signing key.
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"` // bcrypt; never serialized to clients. Empty for AuthSourceLDAP.
	// AuthSource is where this account's credential lives: AuthSourceLocal (a bcrypt hash above) or
	// AuthSourceLDAP (the directory - see internal/authn). It is the account's provenance, not a
	// preference: an LDAP account has no password here and can only ever be authenticated by the
	// directory, and a local account is never claimable by one (see internal/app.Login).
	AuthSource string `json:"auth_source,omitempty"`
	// DisplayName and Email are directory-supplied and cosmetic - they make an admin's user table
	// legible when usernames are sAMAccountNames. Empty for local accounts.
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	// IsAdmin is a LOCAL, seed-only flag: it is written exactly once, by ensureAdmin, and no API can
	// toggle it. That is load-bearing rather than incidental - quota.Allocated excludes admins from
	// the conserved pool, so an admin flag that could flip (say, driven by a directory group) would
	// silently move capacity in and out of the platform's allocation total. Directory rules
	// deliberately grant group roles only, never this.
	IsAdmin bool `json:"is_admin"`
	// Quotas is the capacity granted to this account PER INFRASTRUCTURE, keyed by provider
	// ("kvm", "vsphere"). Capacity is not fungible across infrastructures - a spare core on the
	// KVM host cannot run a vSphere VM - so a single pooled number would let a tenant be admitted
	// against capacity that physically cannot host their cluster. Every admission charges the
	// grant for the provider the cluster is being created on, and nothing else.
	//
	// A missing entry is a zero grant: an account can create clusters only on the infrastructures
	// it has actually been given capacity on. Admins hold no stored grant at all - their budget on
	// each provider is that provider's live unallocated pool (see quota.Budget.Unallocated).
	Quotas map[string]ResourceQuota `json:"quotas,omitempty"`
	// Memberships are the groups this user belongs to, each with its own read/write role (empty =
	// ungrouped). A user can be in several groups at once and hold a different role in each. Group-mates
	// (anyone sharing a group) get access to each other's clusters - read-only or full, per the actor's
	// role in the shared group - see authorizeCluster / authorizeClusterWrite. Admin management only; a
	// user cannot join or leave a group themselves.
	Memberships []GroupMembership `json:"memberships,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// ResourceQuota is an amount of capacity: a grant on one infrastructure, or usage on one.
//
// DiskGB is storage - the sum of every node's ROOT disk plus every extra NodeDisk (see
// quota.ClusterUsage). It is a real, exhaustible resource on the hypervisor's storage pool and the
// one a per-pool root-disk override or an extra disk actually spends, so it is metered like vCPU and
// memory rather than left free. A missing/zero grant means the account can create no clusters on
// that infrastructure at all, since every node has a root disk.
type ResourceQuota struct {
	VCPU   int `json:"vcpu"`
	MemMB  int `json:"mem_mb"`
	DiskGB int `json:"disk_gb"`
}

// QuotaOn returns the user's grant on one infrastructure. An account with no entry for a provider
// has no capacity there - the zero value, not a fallback to some other provider's grant.
func (u *User) QuotaOn(provider string) ResourceQuota {
	return u.Quotas[provider]
}

// RoleIn returns the user's role in the named group and whether they are a member of it. Used by the
// authorization layer to resolve an actor's access to a group-mate's cluster.
func (u *User) RoleIn(groupID string) (GroupRole, bool) {
	for _, m := range u.Memberships {
		if m.GroupID == groupID {
			return m.Role, true
		}
	}
	return "", false
}

// InGroup reports whether the user belongs to the named group (any role).
func (u *User) InGroup(groupID string) bool {
	_, ok := u.RoleIn(groupID)
	return ok
}

// Group is a team. Members of the same group get access to each other's clusters - view-only or
// full (view, scale, delete, promote, kubeconfig, shell), per each member's own role in the group.
// Deleting a group only drops its memberships; it never touches their clusters.
//
// A group is owned either by admins (SourceLocal - created and rostered in the portal, as it always
// was) or by a directory mapping rule (SourceLDAP - see internal/authn). The two coexist: a user can
// hold directory-driven memberships and admin-assigned ones at the same time, and each side only
// ever writes its own (see internal/app.mergeMemberships).
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Source is who owns this group's roster: SourceLocal or SourceLDAP. Immutable after creation -
	// a group cannot change hands, or an admin's group could be captured by a config file.
	Source string `json:"source,omitempty"`
	// SourceKey identifies WHICH directory rule owns the group (the rule's `key`), and is empty for
	// local groups. Groups are keyed on this rather than on Name so that renaming a rule's display
	// name relabels the existing group instead of forking a second one.
	SourceKey string    `json:"source_key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Account provenance (User.AuthSource) and group ownership (Group.Source). The zero value is
// deliberately absent from both: rows written before directory auth existed read back as "" from a
// DEFAULT 'local' column only if someone forgot the default, so the helpers below treat "" as local
// rather than trusting every call site to.
const (
	AuthSourceLocal = "local"
	AuthSourceLDAP  = "ldap"

	SourceLocal = "local"
	SourceLDAP  = "ldap"
)

// FromDirectory reports whether this account is authenticated by a directory rather than by a local
// password. An empty AuthSource is local - that is the pre-directory-auth default.
func (u *User) FromDirectory() bool { return u.AuthSource == AuthSourceLDAP }

// DirectoryManaged reports whether a directory mapping rule owns this group's roster, meaning the
// portal must not let an admin edit it by hand.
func (g *Group) DirectoryManaged() bool { return g.Source == SourceLDAP }

// CustomCatalog is a user-owned, named collection of self-defined Helm-chart add-ons. It is the
// tenant-facing counterpart to the platform's built-in catalog (internal/catalog): a user curates
// their own add-ons and installs them on clusters without a code change. Ownership and sharing
// mirror clusters exactly - the owner and admins have full access, and group-mates share access via
// their per-group read/write role (Read = view, Write = edit; see accessToCatalog in internal/app).
//
// When one of these add-ons is selected onto a cluster its chart definition is COPIED into the
// per-cluster domain.Addon (self-contained, like ValuesOverride), so the untenanted reconcile loop
// never needs to resolve a custom catalog. Deleting a catalog therefore never disturbs clusters that
// already installed its add-ons.
type CustomCatalog struct {
	ID        string        `json:"id"`
	OwnerID   string        `json:"owner_id"`
	Name      string        `json:"name"`
	Addons    []CustomAddon `json:"addons"`
	CreatedAt time.Time     `json:"created_at"`
}

// CustomAddon is one Helm-chart add-on defined inside a CustomCatalog. Chart/Repo/Version identify
// the chart the same way catalog.Addon does - Repo is a classic HTTP chart-repo URL (empty for an
// oci:// chart, whose registry lives in Chart). Values is a full Helm values YAML document (empty =
// the chart's own defaults); it seeds the cluster add-on's ValuesOverride when installed. Namespace
// defaults to Name when empty (the one-namespace-per-add-on convention the helm manager applies).
type CustomAddon struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Chart       string `json:"chart"`
	Version     string `json:"version"`
	Namespace   string `json:"namespace,omitempty"`
	Values      string `json:"values,omitempty"`
}

// CustomAddonRef selects one add-on from a custom catalog, for cluster create/update. It is a
// (catalog, add-on name) pair; the app resolves it against the actor-visible catalogs and copies the
// chart definition onto the cluster (see resolveCustomAddons in internal/app).
type CustomAddonRef struct {
	CatalogID string `json:"catalog_id"`
	Name      string `json:"name"`
}

// Infrastructure providers a cluster can be provisioned on. Which ones a deployment enables
// is env-driven (KAAS_INFRA_PROVIDERS, see internal/app); the value is immutable desired spec
// recorded on the cluster at create time.
const (
	ProviderKVM     = "kvm"
	ProviderVSphere = "vsphere"
	ProviderProxmox = "proxmox"
)

// Cluster is the desired shape plus rolled-up observed status of a cluster.
//
// NOTE: the in-memory store embeds Nodes and Addons on the cluster; the Postgres store
// keeps them in separate child tables (see migrations/0001_init.sql).
type Cluster struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// OwnerID is the User that owns this cluster. Admission charges the owner's quota, and
	// authorization scopes reads/writes to the owner (an admin bypasses the scope). Backfilled to
	// the seeded admin for clusters that predate multi-tenancy (see app.ensureAdmin).
	OwnerID string `json:"owner_id"`
	// Size is the t-shirt size of the CONTROL-PLANE nodes (see Sizes). Worker sizing lives on the
	// node pools - since NodePools landed, every worker draws its shape from its own pool, and this
	// field no longer says anything about them. Immutable after creation.
	Size string `json:"size"`
	// NodePools are the cluster's worker pools: named, independently-scalable groups of worker
	// nodes, each at its own t-shirt size (see NodePool). Every cluster is created with a "default"
	// pool, but nothing keeps it there afterwards - pools are added and removed freely, and a
	// cluster with none is a legal (control-plane-only) shape.
	//
	// This is desired state, like Addons: the API writes the whole list and the reconciler converges
	// to it. Worker COUNT is derived from it (WorkerCount) rather than stored, so the pools are the
	// single writer of the cluster's worker topology.
	//
	// NOTE: the in-memory store embeds these on the cluster; Postgres keeps them in a child table
	// (see migrations/0023_node_pools.sql).
	NodePools []NodePool `json:"node_pools"`

	// NodeDisks are the EXTRA block devices attached to individual worker nodes, beyond their root
	// disk (see NodeDisk). Desired state like NodePools/Addons: the API writes the list, the
	// reconciler converges infrastructure and Ansible onto it.
	//
	// Deliberately NOT on domain.Node: a node row is observed state and is re-created whenever its
	// VM is, so disks hang off the cluster keyed by the node's stable VM NAME - the same reasoning
	// (and shape) as StaticIPs.
	//
	// NOTE: the in-memory store embeds these on the cluster; Postgres keeps them in a child table
	// (see migrations/0024_node_disks.sql).
	NodeDisks []NodeDisk `json:"node_disks,omitempty"`

	// Provider is the infrastructure this cluster is provisioned on (ProviderKVM/ProviderVSphere).
	// Empty means kvm (rows predating multi-provider) - read it through InfraProvider().
	Provider string `json:"provider"`

	// NetworkCIDR is the node network this cluster's VMs sit on. On kvm it is a dedicated,
	// isolated libvirt NAT bridge - one per cluster (see infra/libvirt, internal/netpool), auto-
	// allocated from the platform supernet or user-supplied. On vsphere it is the operator's
	// shared portgroup subnet (the same value for every vsphere cluster). Distinct from the
	// Kubernetes-internal PodCIDR/SvcCIDR below.
	NetworkCIDR string `json:"network_cidr"`
	PodCIDR     string `json:"pod_cidr"`
	SvcCIDR     string `json:"svc_cidr"`

	// vSphere-only network spec, copied from the deployment settings at admission so the
	// reconciler reads desired state from the store, never from mutable env. NetworkName is the
	// portgroup; IPMode is "dhcp" (external DHCP assigns node IPs, discovered via open-vm-tools)
	// or "static" (the platform allocates node IPs from the operator's range). Gateway/DNS and
	// StaticIPs (vm_name → IP, allocation persisted so a re-created node keeps its address)
	// are set only in static mode. All empty on kvm clusters.
	NetworkName string            `json:"network_name,omitempty"`
	IPMode      string            `json:"ip_mode,omitempty"`
	NetGateway  string            `json:"net_gateway,omitempty"`
	NetDNS      string            `json:"net_dns,omitempty"` // comma-separated resolver IPs
	StaticIPs   map[string]string `json:"static_ips,omitempty"`

	// Control-plane topology. ControlPlanes is the number of control-plane VMs: 1 for a
	// single-node control plane, 3 for HA (stacked etcd). For an HA cluster APIVIP holds the
	// floating virtual IP (keepalived) that fronts the API servers via haproxy; the resolved
	// endpoint is APIVIP:8443. Both are empty/1 for a legacy or single-node cluster.
	ControlPlanes int    `json:"control_planes"`
	APIVIP        string `json:"api_vip,omitempty"`

	// LoadBalancerIP is the single node-network address reserved for the cluster's default MetalLB
	// L2 pool, from which the default Envoy Gateway draws its external address (see the default_gateway
	// ansible role / reconcileGatewayWiring). Like APIVIP it is desired state decided once at admission
	// (netpool.LoadBalancerIP on kvm; allocated from the operator range in shared static mode;
	// user-supplied in shared dhcp mode) and kept stable for the cluster's life. Empty on a cluster
	// created before this feature, or one whose metallb/envoy-gateway add-ons were deselected.
	LoadBalancerIP string `json:"load_balancer_ip,omitempty"`

	// DNSDomain is the DNS subdomain this cluster owns - "<name>.kaas.example.internal" - and
	// AppsDomain the domain its apps are published under, "apps.<name>.kaas.example.internal", of
	// which the platform publishes the wildcard "*.<AppsDomain> A LoadBalancerIP" (see internal/dns
	// and reconcileDNSWiring). Like LoadBalancerIP they are desired state derived once, at admission,
	// from deployment config (dns.Settings) and then stored: a later change to KAAS_DNS_BASE_DOMAIN
	// must not move an existing cluster's domain out from under its users. Both empty when the
	// deployment publishes no DNS, or on a cluster created before the feature.
	DNSDomain  string `json:"dns_domain,omitempty"`
	AppsDomain string `json:"apps_domain,omitempty"`

	// Version provenance - the exact set this cluster is running, resolved from a
	// release bundle at create time (see internal/catalog). Recording these
	// is what makes upgrade promotion possible later.
	Bundle     string `json:"bundle"`      // release bundle currently running, e.g. "2026.1"
	OSImage    string `json:"os_image"`    // resolved OS image name, e.g. "ubuntu-26.04"
	K8sVersion string `json:"k8s_version"` // resolved Kubernetes version, e.g. "1.36.2"
	CNI        string `json:"cni"`         // resolved CNI add-on name, e.g. "cilium"
	CNIVersion string `json:"cni_version"` // resolved CNI version pinned by the bundle, e.g. "1.19.5"

	// TargetBundle is the bundle the user asked to promote to (desired). Empty means no upgrade in
	// progress. While TargetBundle != Bundle the reconciler advances the cluster one supersedes hop
	// at a time toward it; Bundle (and the provenance above) is the current, observed set.
	TargetBundle string `json:"target_bundle,omitempty"`

	Phase  Phase  `json:"phase"`
	Status string `json:"status"`

	// generation bumps on every user edit; the reconciler works until
	// ObservedGeneration == Generation. This is the level-triggered signal.
	Generation         int64 `json:"generation"`
	ObservedGeneration int64 `json:"observed_generation"`

	// MonitoringWired records that the cluster's control plane and CNI have already been made
	// Prometheus-scrapeable (see reconcileMonitoringWiring). The wiring - kubeadm manifest edits, a
	// kube-proxy ConfigMap patch and a CNI helm upgrade - is idempotent but not free, so this marker
	// lets the reconciler skip it on unrelated update ticks (e.g. an add-on values edit). It is
	// cleared only when something can actually undo the wiring: a CNI helm (re)install drops the CNI's
	// ServiceMonitor, and rolling a control-plane node onto a fresh golden image regenerates the
	// default (loopback-bound) kubeadm manifests.
	MonitoringWired bool `json:"monitoring_wired,omitempty"`

	// GatewayWired records that the cluster's default MetalLB pool + Envoy Gateway CRs have already
	// been applied (see reconcileGatewayWiring). The wiring is an idempotent `kubectl apply` but not
	// free, so this marker lets the reconciler skip it once done. Unlike MonitoringWired nothing
	// clears it: the CRs live in etcd, so a CNI/OS upgrade or node roll does not undo them.
	GatewayWired bool `json:"gateway_wired,omitempty"`

	// DNSWired records that the cluster's apps wildcard has been published (see reconcileDNSWiring).
	// Like GatewayWired nothing clears it: the record lives in the site's DNS, which no cluster
	// operation touches. It is dropped on delete along with the record itself.
	DNSWired bool `json:"dns_wired,omitempty"`

	// VaultWired records that the cluster's Vault path + policies + the External-Secrets JWT auth role
	// have been provisioned, and the in-cluster ClusterSecretStore applied (see reconcileVaultWiring).
	// Like GatewayWired/DNSWired it guards work decided once - the cluster's KV subtree
	// (kaas/clusters/<id>/*) and its two read/write policies do not move - so a bool latch is right and
	// nothing clears it; it is dropped on delete when releaseVault tears the path down (before the
	// infrastructure, like releaseDNS). The per-USER/GROUP access bindings are NOT gated by this marker:
	// they change as memberships change and are converged separately by the leader-elected
	// vault.Manager.SyncAccess sweep.
	VaultWired bool `json:"vault_wired,omitempty"`

	// StorageWired records which set of node disks has been registered with Longhorn (see
	// reconcileStorageWiring). Unlike GatewayWired/DNSWired this is a FINGERPRINT rather than a bool,
	// and that difference is load-bearing: the gateway's CRs are decided once at admission and never
	// move, but a user adds and removes storage disks on a running cluster, and each change has to
	// reach the node.longhorn.io CRs. A bool would latch after the first disk and silently strand
	// every later one. See StorageFingerprint.
	StorageWired string `json:"storage_wired,omitempty"`

	// StorageDiskGB is the size of the extra disk every WORKER is born with, mounted at
	// LonghornDataPath and registered as that node's Longhorn disk - the storage that backs the
	// cluster's default StorageClass (see DesiredStorageDisks). Chosen at creation and immutable
	// afterwards: a NodeDisk's size cannot change in place, so the way to grow a node's Longhorn
	// capacity is to attach another disk, which is Longhorn's own model too.
	//
	// 0 means the platform provisions no storage disks at all - the shape a user gets by deselecting
	// the longhorn add-on, and what every cluster created before this feature has.
	StorageDiskGB int `json:"storage_disk_gb,omitempty"`

	// CertNotAfter is the OBSERVED earliest expiry of the cluster's kubeadm-managed control-plane
	// certificates (apiserver, etcd, front-proxy, the embedded client certs in admin.conf, …). It is
	// the level-triggered signal for automatic certificate rotation: the reconciler renews when this
	// falls within the renewal window (see reconcile.certRenewCutoff / CertRenewalDue). nil means
	// "not observed yet" - a fresh cluster stamps it at bring-up, and a cluster predating this feature
	// is observed once on its next Ready tick. Renewal (kubeadm certs renew) moves it ~1 year out.
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`

	// Etcd is the OBSERVED state of the cluster's etcd backend store - size, fragmentation, armed
	// alarms - and the level-triggered signal for automatic defragmentation (see EtcdDefragPolicy).
	// nil means never observed. Unlike CertNotAfter this is re-read on a cadence rather than known
	// once: a backend store drifts, a certificate expiry does not.
	Etcd *EtcdStatus `json:"etcd,omitempty"`

	// EtcdSnapshotAt is when the platform last stored a control-plane backup for this cluster (see
	// EtcdSnapshot). It is the level-triggered signal for the next one, held on the cluster row
	// rather than derived from the snapshots table so the due-scan stays one indexed predicate
	// instead of a per-tick aggregate over a table of multi-megabyte rows. nil means never.
	EtcdSnapshotAt *time.Time `json:"etcd_snapshot_at,omitempty"`

	// Repair is the platform's durable memory of what is wrong with this cluster's nodes and what it
	// has already tried about it - the state automatic repair reasons over (see RepairPolicy). It is
	// observed state, not desired: repair converges a cluster TOWARD its desired state and never
	// redefines it, which is why nothing here touches Generation. nil means never observed.
	Repair *ClusterRepair `json:"repair,omitempty"`

	Nodes  []Node  `json:"nodes"`
	Addons []Addon `json:"addons"`

	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// InfraProvider is the infrastructure this cluster runs on, defaulting empty (rows written
// before multi-provider) to kvm.
func (c *Cluster) InfraProvider() string {
	if c.Provider == "" {
		return ProviderKVM
	}
	return c.Provider
}

// NodePool is a named, independently-scalable group of worker nodes, all of one t-shirt size.
// Pools are for WORKERS only - the control plane is not a pool, and its sizing/count stays on the
// cluster (Cluster.Size, Cluster.ControlPlanes).
//
// A pool's Name is load-bearing rather than cosmetic: it is embedded in every one of its nodes' VM
// names ("<cluster>-<pool>-<i>", see DesiredNodes) and published on the Kubernetes node as the
// PoolLabel, so workloads can be steered at a pool with a nodeSelector. That is why ValidatePoolName
// is strict about it - the name has to be simultaneously a legal libvirt/vCenter VM name, a legal
// Linux hostname, and a legal label value.
//
// Size is IMMUTABLE once the pool exists. Changing it in place would mean either rolling every node
// in the pool or resizing running VMs' CPU/memory underneath a live kubelet; the supported path is
// to add a pool at the new size and drain the old one away (the same shape as GKE/EKS node groups).
// DesiredWorkers, by contrast, is the level-triggered scaling signal and is edited freely.
type NodePool struct {
	Name           string `json:"name"`
	Size           string `json:"size"` // t-shirt size of this pool's worker nodes; see Sizes
	DesiredWorkers int    `json:"desired_workers"`
	// DiskGB overrides the ROOT disk size of this pool's workers; 0 means "use the t-shirt size's
	// default" (Sizes[Size].DiskGB). It only ever grows the root disk - ValidateNodePools floors it
	// at the size default, because the node's disk is a copy-on-write clone of the golden image and
	// a volume smaller than the image it clones cannot be created at all.
	//
	// Control planes are unaffected: they are not in a pool and always take Sizes[Cluster.Size].
	//
	// IMMUTABLE once the pool exists, for the same reason Size is. Growing it in place would mean
	// re-creating the libvirt volume, which (via the module's replace_triggered_by) destroys and
	// rebuilds every VM in the pool underneath a live kubelet. The supported path is a new pool at
	// the new size, draining the old away. For storage added to a RUNNING node, see NodeDisk -
	// that is the level-triggered, non-destructive path.
	DiskGB int `json:"disk_gb,omitempty"`
}

// RootDiskGB is the size of this pool's workers' root disk: its override if set, else the t-shirt
// size's default. Reads as 0 for an unknown size, which ValidateNodePools rejects first.
func (p NodePool) RootDiskGB() int {
	if p.DiskGB > 0 {
		return p.DiskGB
	}
	return Sizes[p.Size].DiskGB
}

// MaxRootDiskGB caps a pool's root-disk override. Not a hardware limit - a guard against a typo
// ("4000" for "400") silently eating a hypervisor's storage pool at admission time.
const MaxRootDiskGB = 2000

// DefaultPoolName is the pool every cluster is created with (see app.CreateCluster). It is only a
// default, not a fixture: once the cluster exists this pool is deletable like any other, and the
// name carries no special meaning to the reconciler.
const DefaultPoolName = "default"

// PoolLabel is the Kubernetes node label carrying a node's pool membership, so a workload can be
// pinned to a pool with a nodeSelector ({"kaas.io/nodepool": "gpu"}). It is applied at kubelet
// REGISTRATION via --node-labels (see the ansible worker role), which is what EKS/GKE do: the label
// exists the instant the node first appears, so there is no window in which a pod could land on an
// unlabelled node, and a node rebuilt during an OS roll re-registers with it automatically.
//
// The prefix is deliberately outside the kubernetes.io/k8s.io namespaces, which is what makes a
// kubelet allowed to self-set it (the NodeRestriction admission plugin only polices those two).
//
// SHORTCUT: because the kubelet sets this itself, a compromised node could register into a pool it
// isn't in. Production would use "node-restriction.kubernetes.io/nodepool" - which the kubelet is
// forbidden to self-set - and have the control plane apply it out-of-band after the node registers.
const PoolLabel = "kaas.io/nodepool"

// controlPlaneInfix is the VM-name segment identifying a control-plane node ("<cluster>-cp-<i>").
// It shares a namespace with pool names, so ValidatePoolName reserves it.
const controlPlaneInfix = "cp"

// maxHostnameLen is the Linux hostname ceiling. A node's VM name IS its hostname (injected via
// cloud-init) and its Kubernetes node name, so the fully-formed "<cluster>-<pool>-<i>" must fit.
const maxHostnameLen = 63

// poolNameRE is the DNS-1123 label grammar - the intersection of what a hostname, a libvirt/vCenter
// VM name, and a Kubernetes label value all accept.
var poolNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidatePoolName reports whether name is usable as a pool name on a cluster called clusterName.
// The length check needs the cluster name because the two are concatenated into each node's VM name
// (and therefore its hostname), where the combined result is what actually has to fit.
func ValidatePoolName(clusterName, name string) error {
	if name == "" {
		return fmt.Errorf("node pool name is required")
	}
	if !poolNameRE.MatchString(name) {
		return fmt.Errorf("node pool name %q must be lowercase alphanumeric or '-', and start and end with an alphanumeric", name)
	}
	if name == controlPlaneInfix {
		// "<cluster>-cp-<i>" is the control planes' own naming; a pool called "cp" would mint VM names
		// that collide with them, and the reconciler would see one node claimed by two roles.
		return fmt.Errorf("node pool name %q is reserved for control-plane nodes", name)
	}
	// Worst case is the highest index the pool can reach; budget 3 digits, far past any real pool.
	if n := len(clusterName) + len(name) + len("--999"); n > maxHostnameLen {
		return fmt.Errorf("node pool name %q is too long for cluster %q: node hostnames would reach %d characters (max %d)",
			name, clusterName, n, maxHostnameLen)
	}
	return nil
}

// ValidateNodePools checks a cluster's whole desired pool list: each name usable, each size known,
// counts non-negative, and no duplicate names. An EMPTY list is valid - a cluster may legitimately
// run no workers at all.
func ValidateNodePools(clusterName string, pools []NodePool) error {
	seen := make(map[string]bool, len(pools))
	for _, p := range pools {
		if err := ValidatePoolName(clusterName, p.Name); err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate node pool %q", p.Name)
		}
		seen[p.Name] = true
		spec, ok := Sizes[p.Size]
		if !ok {
			return fmt.Errorf("node pool %q has unknown size %q", p.Name, p.Size)
		}
		if p.DesiredWorkers < 0 {
			return fmt.Errorf("node pool %q: workers must be >= 0, got %d", p.Name, p.DesiredWorkers)
		}
		// The root disk may be grown above the size default but never shrunk below it: the node's
		// volume is a COW clone of the golden image, and libvirt/vSphere both refuse a volume
		// smaller than the image it clones from. Floor it at the default rather than at the image's
		// real virtual size - the default is already >= it, and this keeps the rule explainable.
		if p.DiskGB != 0 {
			if p.DiskGB < spec.DiskGB {
				return fmt.Errorf("node pool %q: disk size must be at least the %q default of %d GB, got %d",
					p.Name, p.Size, spec.DiskGB, p.DiskGB)
			}
			if p.DiskGB > MaxRootDiskGB {
				return fmt.Errorf("node pool %q: disk size must be at most %d GB, got %d", p.Name, MaxRootDiskGB, p.DiskGB)
			}
		}
	}
	return nil
}

// DesiredNode is one VM the cluster should have: what it is called, what it is for, which pool owns
// it, and how big it is.
type DesiredNode struct {
	VMName string
	Role   Role
	Pool   string // "" for a control plane - control planes belong to no pool
	Spec   SizeSpec
}

// DesiredNodes is the single source of the cluster's desired node set - the naming convention AND
// the per-node sizing together, deliberately in one function so the two cannot drift apart.
// Control planes come first ("<cluster>-cp-<i>", sized by Cluster.Size), then each pool's workers in
// pool order ("<cluster>-<pool>-<i>", sized by that pool's own size).
//
// Everything downstream derives from this: the reconciler builds provision.NodeSpecs from it, and
// admission keys its static-IP allocation on the same names. A node's pool is encoded in its NAME,
// which is what lets the rest of the loop stay pool-agnostic - scaling a pool, and deleting one
// outright, are both just "these names are no longer desired", which removedWorkers already handles.
func DesiredNodes(c *Cluster) []DesiredNode {
	cps := c.ControlPlaneCount()
	out := make([]DesiredNode, 0, cps+c.WorkerCount())
	for i := 0; i < cps; i++ {
		out = append(out, DesiredNode{
			VMName: fmt.Sprintf("%s-%s-%d", c.Name, controlPlaneInfix, i),
			Role:   RoleControlPlane,
			Spec:   Sizes[c.Size],
		})
	}
	for _, p := range c.NodePools {
		spec := Sizes[p.Size]
		// The pool's root-disk override (if any) replaces the size's default disk. CPU/memory still
		// come wholesale from the t-shirt size - disk is the one dimension a pool tunes on its own.
		spec.DiskGB = p.RootDiskGB()
		for i := 0; i < p.DesiredWorkers; i++ {
			out = append(out, DesiredNode{
				VMName: fmt.Sprintf("%s-%s-%d", c.Name, p.Name, i),
				Role:   RoleWorker,
				Pool:   p.Name,
				Spec:   spec,
			})
		}
	}
	return out
}

// NodeVMNames is the desired node set reduced to just its VM names, in the same order.
func NodeVMNames(c *Cluster) []string {
	desired := DesiredNodes(c)
	names := make([]string, 0, len(desired))
	for _, d := range desired {
		names = append(names, d.VMName)
	}
	return names
}

// WorkerCount is the cluster's total desired workers across every pool. Derived rather than stored:
// the pools are the single writer of worker topology, so a cached total could only ever disagree
// with them.
func (c *Cluster) WorkerCount() int {
	n := 0
	for _, p := range c.NodePools {
		n += p.DesiredWorkers
	}
	return n
}

// Pool returns the named pool and whether it exists.
func (c *Cluster) Pool(name string) (NodePool, bool) {
	for _, p := range c.NodePools {
		if p.Name == name {
			return p, true
		}
	}
	return NodePool{}, false
}

// NodeSize is the resource shape of one of the cluster's nodes: a control plane takes the cluster's
// own size, a worker its pool's. Falls back to the cluster size for a worker whose pool has since
// been deleted (its VM is on its way out, but telemetry may still describe it).
func (c *Cluster) NodeSize(n Node) SizeSpec {
	if n.Role == RoleWorker {
		if p, ok := c.Pool(n.Pool); ok {
			spec := Sizes[p.Size]
			spec.DiskGB = p.RootDiskGB() // the pool's root-disk override, as DesiredNodes applies it
			return spec
		}
	}
	return Sizes[c.Size]
}

// ControlPlaneCount is the number of control-plane VMs this cluster should have. It falls
// back to the size's default (1) for legacy clusters written before the field existed.
func (c *Cluster) ControlPlaneCount() int {
	if c.ControlPlanes > 0 {
		return c.ControlPlanes
	}
	return Sizes[c.Size].ControlPlanes
}

// HA reports whether this cluster runs a highly-available (multi-node) control plane.
func (c *Cluster) HA() bool { return c.ControlPlaneCount() > 1 }

// APIEndpoint is the control-plane endpoint kubeadm advertises. For an HA cluster it's the
// keepalived VIP fronted by haproxy (APIVIP:8443); empty otherwise (kubeadm then uses the
// single control-plane node's own address).
func (c *Cluster) APIEndpoint() string {
	if c.APIVIP == "" {
		return ""
	}
	return c.APIVIP + ":8443"
}

// NeedsWork reports whether the reconciler has outstanding work for this cluster.
func (c *Cluster) NeedsWork() bool {
	if c.Phase.Terminal() {
		return false
	}
	return c.Phase != PhaseReady || c.ObservedGeneration != c.Generation
}

// UpgradePending reports whether the cluster has an outstanding bundle promotion - a target that
// differs from the bundle it is currently running.
func (c *Cluster) UpgradePending() bool {
	return c.TargetBundle != "" && c.TargetBundle != c.Bundle
}

// CertRenewalDue reports whether automatic certificate rotation should act on this cluster now. It
// is deliberately narrow: only a Ready, converged cluster qualifies, so renewal never races another
// in-flight transition (an upgrade/update already renews or reissues certs). A cluster whose expiry
// has never been observed (CertNotAfter == nil) qualifies too - that's the one-time backfill signal
// for clusters that predate the feature; the Ready-tick observes and stamps it, only renewing if it
// turns out to be within the window. cutoff is now + the renewal window (see reconcile.certRenewCutoff);
// callers pass it only when rotation is enabled.
func (c *Cluster) CertRenewalDue(cutoff time.Time) bool {
	if c.Phase != PhaseReady || c.ObservedGeneration != c.Generation {
		return false
	}
	return c.CertNotAfter == nil || c.CertNotAfter.Before(cutoff)
}

// Node is one VM.
type Node struct {
	ID     string `json:"id"`
	Role   Role   `json:"role"`
	VMName string `json:"vm_name"`
	// Pool is the NodePool that owns this node; empty for a control plane. Observed state, derived
	// from the desired node set at provision time (see DesiredNodes) - a node never changes pool,
	// because its pool is baked into its VM name.
	Pool  string `json:"pool,omitempty"`
	IP    string `json:"ip"`
	MAC   string `json:"mac"`
	Phase string `json:"phase"`
	// Image is the golden image the VM was cloned from (see catalog.GoldenImageName). Rolling
	// node replacement compares this against the target bundle's image to know which VMs still
	// run the old OS/Kubernetes, so it can resume idempotently one node at a time.
	Image string `json:"image,omitempty"`
}

// NodeDisk is one EXTRA block device attached to a single node, beyond its root disk: desired
// state, keyed on the node's VM name, formatted and mounted by Ansible via LVM.
//
// Why VMName and not a node ID: the node row is OBSERVED state, re-created whenever its VM is (a
// rolling OS replacement destroys and rebuilds the node, minting a new ID). The VM NAME is the
// stable identity - it is what DesiredNodes mints and what the provisioner converges on - so keying
// disks on it means a node rebuilt underneath keeps its disks. This mirrors Cluster.StaticIPs.
//
// Phase follows the Addon idiom exactly, and for the same reason - the work is not instantaneous
// and must survive a crash mid-flight:
//
//	pending  -> the disk is desired; the volume may not exist and is certainly not mounted yet.
//	attached -> the volume exists, is attached to the VM, and is mounted at MountPath.
//	removing -> the user asked for it back; it must be unmounted and its volume group torn down
//	            BEFORE the volume is detached, or the node keeps a stale mount over a vanished
//	            device. The reconciler drives that ordering (see reconcileNodeDisks) and only then
//	            drops the row, which is what finally lets the provisioner destroy the volume.
//
// A disk is never mutated in place: SizeGB and MountPath are immutable once it exists (growing an
// LVM volume online is real work this platform does not do - see ValidateNodeDisks).
type NodeDisk struct {
	// VMName is the node this disk belongs to (domain.Node.VMName / DesiredNode.VMName).
	VMName string `json:"vm_name"`
	// Name is the disk's logical name, unique per NODE (not per cluster). It names the volume, the
	// LVM volume group ("kaas-<name>") and the logical volume, so it is DNS-1123 like a pool name.
	Name string `json:"name"`
	// SizeGB is the raw device size. Immutable.
	SizeGB int `json:"size_gb"`
	// MountPath is the absolute path the filesystem is mounted at, e.g. "/var/lib/data". Immutable.
	MountPath string `json:"mount_path"`
	// FSType is the filesystem to lay down: FSExt4 or FSXFS. Immutable.
	FSType string `json:"fs_type"`
	Phase  string `json:"phase"`
	// WWN is the disk's stable hardware identity, minted by the platform at admission (NewDiskWWN)
	// and handed to BOTH the infrastructure (which pins it on the virtual disk) and Ansible (which
	// finds the device at /dev/disk/by-id/wwn-<WWN>). Guest device names (/dev/vdb, /dev/sdc) are
	// assigned in attach order and shift when a disk is removed, so they are unusable as identity -
	// this is what makes "format the RIGHT disk" safe. Empty on a vsphere disk, where the platform
	// cannot choose the identity and reads it back instead (see DeviceID).
	WWN string `json:"wwn,omitempty"`
	// DeviceID is the OBSERVED identity of the disk inside the guest, reported by the provisioner
	// once the disk exists: a hex token that appears in the /dev/disk/by-id/ symlink udev creates
	// for it. On kvm it is the WWN we pinned ("5000c50…", landing in .../wwn-0x5000c50…); on vsphere
	// it is the VMDK's UUID, which vCenter mints and the module reads back.
	//
	// It is a token to MATCH rather than a full path because the two providers spell the by-id entry
	// differently. Resolving by match (see the node_disks role) is what lets one Ansible role serve
	// both, and is why nothing above the provision seam special-cases a provider.
	//
	// Empty until the disk has actually been created; the reconciler will not run the format/mount
	// step for a disk with no DeviceID, because it would have no safe way to pick the device.
	DeviceID string `json:"device_id,omitempty"`
}

// Extra-disk phases. See NodeDisk.
const (
	DiskPhasePending  = "pending"
	DiskPhaseAttached = "attached"
	DiskPhaseRemoving = "removing"
)

// Filesystems a NodeDisk may be formatted with. Both are in the golden image.
const (
	FSExt4 = "ext4"
	FSXFS  = "xfs"
)

// Extra-disk size bounds. The floor is a practical LVM minimum (a PV needs room for metadata); the
// ceiling is the same typo guard as MaxRootDiskGB.
const (
	MinDiskGB = 1
	MaxDiskGB = 2000
)

// MaxDisksPerNode caps extra disks on one node. virtio-scsi addresses far more, but this is a demo
// platform on a laptop - the cap makes a runaway loop in a client fail loudly at admission.
const MaxDisksPerNode = 8

// NewDiskWWN mints the stable WWN for one node's disk, deterministically from the cluster, the VM
// name and the disk name - so it is identical every time it is computed and never has to be stored
// to be reproduced. The 0x5000c50 prefix is a well-formed NAA-5 IEEE-registered header (libvirt
// requires exactly 16 hex digits and rejects anything else).
//
// SHORTCUT: 9 hex digits of MD5 is 36 bits of the identity. Two disks colliding inside one cluster
// would be a genuine "format the wrong device" bug, but at this platform's scale (MaxDisksPerNode
// per node) the odds are negligible. Production would allocate identities rather than hash them.
func NewDiskWWN(clusterID, vmName, diskName string) string {
	sum := md5.Sum([]byte(clusterID + "/" + vmName + "/" + diskName))
	return "0x5000c50" + hex.EncodeToString(sum[:])[:9]
}

// VolumeGroup is the LVM volume group this disk's PV is placed in, and LogicalVolume the single LV
// carved from it. One VG per disk (rather than one shared VG per node) is deliberate: it keeps each
// disk independently removable - tearing one down can never strand an LV that spans another disk's
// extents - which is exactly what the removing phase needs.
func (d NodeDisk) VolumeGroup() string   { return "kaas-" + d.Name }
func (d NodeDisk) LogicalVolume() string { return "data" }

// DisksFor returns the cluster's extra disks for one node, in stable name order.
func DisksFor(c *Cluster, vmName string) []NodeDisk {
	var out []NodeDisk
	for _, d := range c.NodeDisks {
		if d.VMName == vmName {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// The platform's default storage: every worker is born with one extra disk, formatted and mounted
// like any other, which the bundled longhorn add-on registers as that node's Longhorn disk. That is
// the whole mechanism - Longhorn wants a MOUNTED DIRECTORY, not a raw device, so the extra-disk
// machinery that already exists is exactly what it needs and there is no second storage concept.
const (
	// LonghornDataPath is the mount point Longhorn's chart is pointed at (defaultSettings.
	// defaultDataPath), and so the mount point of the platform's own per-worker storage disk.
	LonghornDataPath = "/var/lib/longhorn"
	// PlatformStorageDiskName is the reserved name of that disk. It is derived from the cluster's
	// StorageDiskGB rather than added by a user, so ValidateDiskName refuses it on the add-disk
	// endpoint - a user-created disk by that name would be indistinguishable from the platform's and
	// would fight DesiredStorageDisks on every admission.
	PlatformStorageDiskName = "storage"
	// DefaultStorageDiskGB is what a cluster gets when the caller says nothing. Small on purpose:
	// this is a demo platform on a laptop, and the disk is charged to the owner's quota per worker.
	DefaultStorageDiskGB = 10
	// MaxLonghornReplicas caps the derived replica factor (see LonghornReplicas).
	MaxLonghornReplicas = 3
)

// LonghornMountPath is where a disk that feeds the storage pool is mounted: the platform's own disk
// takes LonghornDataPath itself (which is the chart's default data path, so it registers with no
// configuration at all), and every additional one takes a sibling path. Longhorn supports many disks
// per node and simply sums their capacity, which is what lets a pool grow by attaching disks rather
// than by growing a volume group - so each disk keeps its own VG and stays independently removable.
func LonghornMountPath(diskName string) string {
	if diskName == PlatformStorageDiskName {
		return LonghornDataPath
	}
	return LonghornDataPath + "-" + diskName
}

// FeedsStoragePool reports whether this disk is registered with Longhorn as pool capacity, decided
// by WHERE IT IS MOUNTED rather than by a flag on the row.
//
// That is deliberate. The alternative - a purpose column - would be a second, invisible truth about
// a disk that the user can already see the answer to: a disk mounted under the Longhorn data path
// is Longhorn's, and a disk mounted anywhere else is an ordinary filesystem the node's workloads can
// use directly (a legitimate escape hatch, and the shape every disk created before this feature has).
// The portal defaults a new disk's mount path to LonghornMountPath, so the common path needs no
// explanation; the uncommon one stays possible.
func (d NodeDisk) FeedsStoragePool() bool {
	return d.MountPath == LonghornDataPath || strings.HasPrefix(d.MountPath, LonghornDataPath+"-")
}

// IsPlatformStorage reports whether this is the platform-derived per-worker storage disk, which the
// API refuses to delete individually: it is a function of the cluster's StorageDiskGB, so removing
// the row would only make the next admission mint it again.
func (d NodeDisk) IsPlatformStorage() bool { return d.Name == PlatformStorageDiskName }

// DesiredStorageDisks is the platform's own storage disks for a cluster: one per desired WORKER,
// sized StorageDiskGB. Like every other derivation of the node set it goes through DesiredNodes, so
// a pool added, scaled or deleted moves the storage disks with it and nothing re-derives VM names.
//
// It returns the disks the cluster SHOULD have; merging them into the ones it already has (without
// disturbing their observed phase/DeviceID) is app.syncStorageDisks's job.
func DesiredStorageDisks(c *Cluster) []NodeDisk {
	if c.StorageDiskGB <= 0 {
		return nil
	}
	var out []NodeDisk
	for _, n := range DesiredNodes(c) {
		if n.Role != RoleWorker {
			continue
		}
		out = append(out, NodeDisk{
			VMName:    n.VMName,
			Name:      PlatformStorageDiskName,
			SizeGB:    c.StorageDiskGB,
			MountPath: LonghornMountPath(PlatformStorageDiskName),
			FSType:    FSExt4,
			Phase:     DiskPhasePending,
			WWN:       NewDiskWWN(c.ID, n.VMName, PlatformStorageDiskName),
		})
	}
	return out
}

// LonghornReplicas is the replica factor the platform configures a cluster's Longhorn with: one
// replica per worker, capped at MaxLonghornReplicas and never below 1.
//
// Derived rather than fixed because both ends hurt: the chart's default of 3 leaves every volume on
// a two-worker cluster permanently degraded (Longhorn cannot place two replicas on one node), while
// a flat 1 gives up the very property that makes Longhorn worth its overhead here - a volume that
// survives losing a node. It is resolved at INSTALL time (see app.longhornExtras) and does not
// follow a later scale; production would reconcile the setting on every topology change.
func LonghornReplicas(c *Cluster) int {
	n := c.WorkerCount()
	if n > MaxLonghornReplicas {
		n = MaxLonghornReplicas
	}
	if n < 1 {
		n = 1
	}
	return n
}

// NeedsLonghornRegistration reports whether the platform has to tell Longhorn about this disk.
//
// The platform's OWN storage disk is mounted at LonghornDataPath, which is the chart's
// defaultDataPath - longhorn-manager discovers it and writes its own default disk entry onto the
// node's CR unprompted. Registering it a second time is not merely redundant, it is an ERROR:
// Longhorn rejects two disks on one node sharing a path. So the wiring step deals only with the
// ADDITIONAL disks a user attaches, and the common cluster needs no wiring at all.
//
// Only ATTACHED disks count: a pending disk has no filesystem yet, and pointing Longhorn at a path
// that is still the root disk would have it schedule replicas onto the wrong device.
func (d NodeDisk) NeedsLonghornRegistration() bool {
	return d.Phase == DiskPhaseAttached && d.FeedsStoragePool() && !d.IsPlatformStorage()
}

// LonghornDiskName is the key this disk takes in the node CR's spec.disks map. Namespaced so it can
// never collide with the "default-disk-<hash>" entry longhorn-manager mints for the data path.
func (d NodeDisk) LonghornDiskName() string { return "kaas-" + d.Name }

// StorageFingerprint identifies the set of disks Longhorn should currently have registered, as
// "<vm>:<path>" pairs. It is what Cluster.StorageWired stores, so the wiring step re-runs exactly
// when that set changes and is skipped on every other tick - the difference from GatewayWired's
// plain bool, and the reason a disk attached to a long-Ready cluster still reaches Longhorn.
func StorageFingerprint(c *Cluster) string {
	var parts []string
	for _, d := range c.NodeDisks {
		if d.NeedsLonghornRegistration() {
			parts = append(parts, d.VMName+":"+d.MountPath)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// StorageDisksFor is one node's Longhorn-registered disks, in stable mount-path order - what the
// longhorn_disks role writes onto that node's node.longhorn.io CR.
func StorageDisksFor(c *Cluster, vmName string) []NodeDisk {
	var out []NodeDisk
	for _, d := range DisksFor(c, vmName) {
		if d.NeedsLonghornRegistration() {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MountPath < out[j].MountPath })
	return out
}

// ValidateNodeDisks checks a cluster's whole desired extra-disk list against the node set it is
// allowed to name. Disks may only sit on WORKER nodes that the cluster actually desires: a control
// plane's storage is the platform's business (etcd lives there), and a disk pinned to a VM name that
// DesiredNodes does not mint would be desired state nothing ever converges.
func ValidateNodeDisks(c *Cluster, disks []NodeDisk) error {
	workers := make(map[string]bool)
	for _, d := range DesiredNodes(c) {
		if d.Role == RoleWorker {
			workers[d.VMName] = true
		}
	}
	perNode := map[string]int{}
	seen := map[string]bool{}
	mounts := map[string]bool{}
	for _, d := range disks {
		if !workers[d.VMName] {
			return fmt.Errorf("disk %q: node %q is not a worker node of this cluster", d.Name, d.VMName)
		}
		if err := ValidateDiskName(d.Name); err != nil {
			return err
		}
		key := d.VMName + "/" + d.Name
		if seen[key] {
			return fmt.Errorf("duplicate disk %q on node %q", d.Name, d.VMName)
		}
		seen[key] = true
		perNode[d.VMName]++
		if perNode[d.VMName] > MaxDisksPerNode {
			return fmt.Errorf("node %q: at most %d extra disks per node", d.VMName, MaxDisksPerNode)
		}
		if d.SizeGB < MinDiskGB || d.SizeGB > MaxDiskGB {
			return fmt.Errorf("disk %q: size must be between %d and %d GB, got %d", d.Name, MinDiskGB, MaxDiskGB, d.SizeGB)
		}
		if d.FSType != FSExt4 && d.FSType != FSXFS {
			return fmt.Errorf("disk %q: filesystem must be %q or %q, got %q", d.Name, FSExt4, FSXFS, d.FSType)
		}
		if err := ValidateMountPath(d.MountPath); err != nil {
			return fmt.Errorf("disk %q: %w", d.Name, err)
		}
		// Two disks on one node mounted at the same path: the second would shadow the first, and
		// the node would silently be a disk down. Across DIFFERENT nodes the same path is normal
		// (that is the usual shape - every node in a pool mounting /var/lib/data).
		mkey := d.VMName + "/" + d.MountPath
		if mounts[mkey] {
			return fmt.Errorf("node %q: two disks both mount %q", d.VMName, d.MountPath)
		}
		mounts[mkey] = true
	}
	return nil
}

// diskNameRE is DNS-1123, as for pool names: the disk's name becomes an LVM volume-group name and a
// libvirt volume name, so it has to be conservative.
var diskNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// maxDiskNameLen keeps "kaas-<name>" a comfortable LVM volume-group name.
const maxDiskNameLen = 32

// ValidateDiskName reports whether name is usable as an extra disk's name.
func ValidateDiskName(name string) error {
	if name == "" {
		return fmt.Errorf("disk name is required")
	}
	if !diskNameRE.MatchString(name) {
		return fmt.Errorf("disk name %q must be lowercase alphanumeric or '-', and start and end with an alphanumeric", name)
	}
	if len(name) > maxDiskNameLen {
		return fmt.Errorf("disk name %q is too long (max %d characters)", name, maxDiskNameLen)
	}
	return nil
}

// ValidateUserDiskName is ValidateDiskName plus the reserved-name check, for disks a USER asks for.
// The two are separate because the platform mints a disk by the reserved name itself
// (DesiredStorageDisks) and that same list is then checked by ValidateNodeDisks - so the reservation
// cannot live in the shared validator without the platform's own disks failing it.
func ValidateUserDiskName(name string) error {
	if err := ValidateDiskName(name); err != nil {
		return err
	}
	if name == PlatformStorageDiskName {
		return fmt.Errorf("disk name %q is reserved for the platform's per-worker storage disk", name)
	}
	return nil
}

// protectedMounts are paths an extra disk must never be mounted over. Mounting a fresh, empty
// filesystem on any of these hides the running system underneath it - /etc would sever the node's
// identity, /var/lib/kubelet or /etc/kubernetes would break the kubelet on the spot, and / is not a
// thing a second disk can be. This is the one validation here that prevents a user destroying their
// own node with a plausible-looking request.
var protectedMounts = []string{"/", "/boot", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64",
	"/dev", "/proc", "/sys", "/run", "/var", "/var/lib", "/var/lib/kubelet", "/var/lib/containerd",
	"/var/lib/etcd", "/etc/kubernetes", "/home", "/root", "/tmp"}

// ValidateMountPath reports whether an extra disk may be mounted at path. It must be absolute,
// clean, and not one of the system paths a fresh filesystem would shadow (see protectedMounts). A
// path BELOW a protected one is fine and is the normal case - /var/lib/data shadows nothing.
func ValidateMountPath(path string) error {
	if path == "" {
		return fmt.Errorf("mount path is required")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("mount path %q must be absolute", path)
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("mount path %q must be a clean path (no trailing slash, no '.' or '..')", path)
	}
	for _, p := range protectedMounts {
		if path == p {
			return fmt.Errorf("mount path %q is a system directory and cannot be used for an extra disk", path)
		}
	}
	return nil
}

// Addon is a requested cluster add-on (catalog-as-data; see internal/addons).
type Addon struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Phase   string `json:"phase"`
	// ValuesOverride is a per-cluster, full Helm values document (YAML) that replaces the curated
	// catalog defaults for this add-on. Empty means "use the catalog's --set values" (the default).
	// When set, the reconciler installs the add-on with `helm ... -f <override>` and skips the
	// catalog --set flags - the override was seeded from (chart defaults + catalog), so it is
	// self-contained. Editing it flips Phase to "updating" so the reconciler re-runs helm upgrade.
	// For a custom add-on (CatalogID set) it carries the add-on's own values (no catalog --set exists).
	ValuesOverride string `json:"values_override,omitempty"`

	// The fields below make a CUSTOM add-on (one defined in a user's CustomCatalog) self-contained,
	// so the untenanted reconcile loop installs it straight from this record without resolving the
	// owner's catalog. All empty for a built-in catalog add-on, whose chart/repo/namespace the helm
	// manager resolves by Name from internal/catalog. An add-on is "custom" iff CatalogID != "".
	CatalogID   string `json:"catalog_id,omitempty"`  // origin CustomCatalog (provenance; UI grouping)
	Chart       string `json:"chart,omitempty"`       // Helm chart ref (oci:// or chart name)
	Repo        string `json:"repo,omitempty"`        // classic HTTP chart-repo URL (empty for oci://)
	Namespace   string `json:"namespace,omitempty"`   // install namespace (empty = same as Name)
	Description string `json:"description,omitempty"` // human summary, shown in the Add-ons tab
}

// Custom reports whether this cluster add-on came from a user's custom catalog (as opposed to the
// platform's built-in catalog). Custom add-ons are self-contained - the helm manager installs them
// from this record rather than resolving internal/catalog by name.
func (a Addon) Custom() bool { return a.CatalogID != "" }

// NodeMetrics is a point-in-time resource-usage sample for one node, as reported by
// metrics-server (metrics.k8s.io). Usage is the live consumption; Capacity is the node's
// allocatable total, so the UI can render a percentage. CPU is in millicores, memory in bytes -
// the units the Kubernetes metrics API uses.
type NodeMetrics struct {
	NodeName         string `json:"node_name"`
	CPUUsedMilli     int64  `json:"cpu_used_milli"`
	CPUCapacityMilli int64  `json:"cpu_capacity_milli"`
	MemUsedBytes     int64  `json:"mem_used_bytes"`
	MemCapacityBytes int64  `json:"mem_capacity_bytes"`
}

// MetricsSnapshot is the latest resource-usage reading for a cluster: one NodeMetrics per node,
// plus the time it was collected. It is live/observed telemetry, not desired state - collected
// worker-side from the in-cluster metrics API and served read-through (see internal/metrics).
// The cluster-wide totals are the sum of the per-node samples; the UI aggregates them.
type MetricsSnapshot struct {
	ClusterID   string        `json:"cluster_id"`
	CollectedAt time.Time     `json:"collected_at"`
	Nodes       []NodeMetrics `json:"nodes"`
}

// HealthStatus is the outcome of a health check, or the rolled-up health of a whole cluster. It
// is an observed axis orthogonal to the lifecycle Phase - a Ready cluster can be Degraded.
// "unknown" means a check couldn't be evaluated (e.g. etcd quorum on a single-node cluster, or
// add-on availability with no add-ons installed); it is excluded from the rollup rather than
// counted against it.
type HealthStatus string

const (
	HealthUnknown   HealthStatus = "unknown"
	HealthHealthy   HealthStatus = "healthy"
	HealthDegraded  HealthStatus = "degraded"
	HealthUnhealthy HealthStatus = "unhealthy"
)

// HealthCheck is one evaluated aspect of a cluster's health (API server reachable, nodes Ready,
// system workloads, scheduling capacity, etcd quorum, add-on availability). ID is a stable slug
// the UI keys on; Summary is a human one-liner ("3/3 nodes Ready").
type HealthCheck struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Status  HealthStatus `json:"status"`
	Summary string       `json:"summary"`
}

// NodeHealth is per-node health detail carried alongside the checks, so the UI can show a
// Ready/pressure/cordon indicator per node on the Nodes tab. Pressures lists any active node
// pressure conditions (MemoryPressure/DiskPressure/PIDPressure) - empty when the node is
// unpressured. Cordoned reports spec.unschedulable - set by `kubectl cordon` (or drain) - which
// leaves Ready untouched, so it needs its own signal rather than riding on Ready.
type NodeHealth struct {
	NodeName  string   `json:"node_name"`
	Ready     bool     `json:"ready"`
	Cordoned  bool     `json:"cordoned,omitempty"`
	Pressures []string `json:"pressures,omitempty"`
}

// HealthSnapshot is the latest health reading for a cluster: a set of individual checks, a
// rolled-up Status (the worst non-unknown check), and per-node detail. Like MetricsSnapshot it is
// live/observed telemetry, not desired state - evaluated worker-side against the cluster and
// served read-through (only the worker can reach the cluster network; see internal/health). It is
// deliberately decoupled from the reconcile loop: health never changes a cluster's Phase.
type HealthSnapshot struct {
	ClusterID   string        `json:"cluster_id"`
	Status      HealthStatus  `json:"status"`
	Checks      []HealthCheck `json:"checks"`
	Nodes       []NodeHealth  `json:"nodes"`
	CollectedAt time.Time     `json:"collected_at"`
}

// healthRank orders the three real statuses so RollupHealth can pick the worst. Unknown is not
// ranked - it is skipped entirely (see RollupHealth).
var healthRank = map[HealthStatus]int{HealthHealthy: 1, HealthDegraded: 2, HealthUnhealthy: 3}

// RollupHealth returns the worst status among the checks, ignoring "unknown" (a check that
// couldn't be evaluated shouldn't drag the cluster to Degraded). With no evaluable checks it
// returns HealthUnknown.
func RollupHealth(checks []HealthCheck) HealthStatus {
	worst := HealthUnknown
	for _, c := range checks {
		if c.Status == HealthUnknown {
			continue
		}
		if worst == HealthUnknown || healthRank[c.Status] > healthRank[worst] {
			worst = c.Status
		}
	}
	return worst
}

// OperationKind classifies a user-initiated change to a cluster, for the audit/history log.
// It is higher-level than an events.Event (which is the per-tick reconciler timeline): one
// Operation records one intent ("scale workers 2 → 3", "upgrade to 2026.1") and is marked
// finished when the reconciler converges the cluster to the generation it produced.
type OperationKind string

const (
	OpCreate  OperationKind = "create"  // cluster created
	OpScale   OperationKind = "scale"   // worker count changed
	OpAddons  OperationKind = "addons"  // add-on set changed
	OpUpgrade OperationKind = "upgrade" // bundle promotion (Kubernetes / OS / CNI / add-on versions)
	OpDisks   OperationKind = "disks"   // a node's extra disks changed
	// OpSSH is a browser SSH session to a node (internal/nodessh) - an audit record, NOT a
	// desired-state change: it produces no generation and is completed on disconnect, not by the
	// reconciler. Its Detail holds the best-effort list of commands typed during the session. Because
	// its lifecycle is request-driven it is deliberately EXCLUDED from the reconciler's
	// generation-based completion sweep (see store.CompleteOperations).
	OpSSH OperationKind = "ssh"

	// The PLATFORM-initiated kinds below record the automated maintenance and repair the control
	// plane performs on its own - the level-triggered counterpart of the user-initiated kinds above.
	// Unlike those, they are written by the RECONCILER rather than the app, carry no user actor and no
	// generation, and are opened and closed within the single phase that performs the work (like
	// OpSSH, and for the same reason: they never bump the cluster's generation, so the sweep's
	// "observed_generation caught up" signal can never apply to them - see SweepExempt).
	OpRepair      OperationKind = "repair"       // an auto-repair ladder action (power-on / rejoin / restart-kubelet / rebuild)
	OpRestore     OperationKind = "restore"      // a sole-control-plane rebuild from a stored etcd snapshot (repair's lossy last rung)
	OpSnapshot    OperationKind = "snapshot"     // a periodic sealed etcd/control-plane backup
	OpCertRenewal OperationKind = "cert-renewal" // automatic control-plane certificate rotation
	OpDefrag      OperationKind = "defrag"       // automatic etcd defragmentation
)

// SweepExempt reports whether an operation closes itself rather than being closed by the reconciler's
// generation-based completion sweep (store.CompleteOperations). True for the request-driven SSH
// session and for every platform-initiated maintenance/repair action - none of which bump the
// cluster's generation, so the sweep's "observed_generation caught up" trigger never fires for them
// and would otherwise leave them in_progress forever.
func (k OperationKind) SweepExempt() bool {
	switch k {
	case OpSSH, OpRepair, OpRestore, OpSnapshot, OpCertRenewal, OpDefrag:
		return true
	}
	return false
}

// SweepExemptKinds is the set SweepExempt returns true for, for the store's completion-sweep query
// (which needs the list as data rather than a predicate it cannot express in SQL).
func SweepExemptKinds() []OperationKind {
	return []OperationKind{OpSSH, OpRepair, OpRestore, OpSnapshot, OpCertRenewal, OpDefrag}
}

// OperationStatus is where an operation is in its lifecycle. There is no explicit "failed" -
// a stuck reconcile leaves the operation in_progress (the Activity tab shows the error events),
// and it flips to completed once the cluster converges.
type OperationStatus string

const (
	OpInProgress OperationStatus = "in_progress"
	OpCompleted  OperationStatus = "completed"
)

// Operation is one entry in a cluster's action history: who did what, when, and (for upgrades
// and scaling) between which values. Recorded by the app when it writes desired state, and
// completed by the reconciler when observed_generation catches up to the recording generation.
type Operation struct {
	ID         string          `json:"id"`
	ClusterID  string          `json:"cluster_id"`
	Kind       OperationKind   `json:"kind"`
	Summary    string          `json:"summary"`          // human-readable one-liner
	Detail     string          `json:"detail,omitempty"` // optional extra context (e.g. per-component upgrade diff)
	Generation int64           `json:"generation"`       // the cluster generation this operation produced
	Status     OperationStatus `json:"status"`
	// ActorID/ActorUsername identify who triggered this operation. Denormalized (the username is
	// copied at write time, not joined from the users table) so the audit trail stays meaningful
	// even after the actor is later deleted or renamed - like any audit log entry.
	ActorID       string     `json:"actor_id,omitempty"`
	ActorUsername string     `json:"actor_username,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// SizeSpec maps a t-shirt size to concrete per-node resources and control-plane count.
type SizeSpec struct {
	CPUs          int
	MemMB         int
	DiskGB        int
	ControlPlanes int
}

// Sizes is the t-shirt-size catalog. Defaults keep control planes at 1.
//
// Memory floors are set by the bundle's own add-ons, not by any user workload: every cluster runs
// the kube-prometheus-stack (Prometheus/Grafana/Alertmanager/kube-state-metrics/node-exporter) AND
// the trivy-operator, whose image-scan jobs are memory-hungry. On a single-worker cluster all of it
// lands on one node (the control plane is tainted), so ~1.2 GiB of add-on requests plus a Trivy scan
// (~0.5–1.5 GiB, transient) must fit. 2 GiB - the original `small` - OOM-thrashed under exactly this
// load (the whole node death-spiralled on liveness-probe timeouts), so `small` starts at 8 GiB.
var Sizes = map[string]SizeSpec{
	"small":  {CPUs: 2, MemMB: 8192, DiskGB: 50, ControlPlanes: 1},
	"medium": {CPUs: 4, MemMB: 16384, DiskGB: 50, ControlPlanes: 1},
	"large":  {CPUs: 8, MemMB: 32768, DiskGB: 50, ControlPlanes: 1},
}
