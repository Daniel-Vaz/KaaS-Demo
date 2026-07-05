// TypeScript mirrors of the Go control-plane JSON (snake_case; see internal/domain and
// internal/catalog). Keep these in sync with the API - they are the contract the portal renders.

export type Phase =
  | 'Pending'
  | 'ProvisioningInfra'
  | 'InfraReady'
  | 'ControlPlaneReady'
  | 'WorkersReady'
  | 'InstallingAddons'
  | 'Ready'
  | 'Updating'
  | 'Upgrading'
  | 'RenewingCerts'
  | 'DefragmentingEtcd'
  | 'SnapshottingEtcd'
  | 'Repairing'
  | 'Deleting'
  | 'Deleted'
  | 'Failed';

export type Role = 'control-plane' | 'worker';

export interface Node {
  id: string;
  role: Role;
  vm_name: string;
  // The node pool this node belongs to; absent for a control plane (which belongs to none).
  pool?: string;
  ip: string;
  mac: string;
  phase: string;
  image?: string; // golden image the VM was cloned from (see catalog.GoldenImageName)
}

export interface Addon {
  name: string;
  version: string;
  phase: string; // pending | installing | installed | updating | removing | ...
  values_override?: string; // per-cluster Helm values override (YAML); empty = curated catalog defaults
  // Set only for a CUSTOM add-on (one from a user's custom catalog): its self-contained chart
  // definition, copied onto the cluster. catalog_id groups installed custom add-ons by origin.
  catalog_id?: string;
  chart?: string;
  repo?: string;
  namespace?: string;
  description?: string;
}

// AddonValuesView backs the in-browser values editor: the chart's own defaults, the platform's
// curated catalog overrides, and the two merged (the editor's seed). override/phase are present only
// for a cluster-scoped view of an installed add-on.
export interface AddonValuesView {
  addon: string;
  version: string;
  chart_values: string;
  catalog_overrides?: Record<string, string>;
  effective_values: string;
  override?: string;
  phase?: string;
}

// ---- users / tenancy ----
// Local accounts (see internal/domain.User, internal/auth). A normal user owns and sees only their
// own clusters; an admin manages users and sees all clusters. Quota is per-user, carved from the
// platform total under a conserved-pool invariant.

// GroupRole is a user's coarse read/write RBAC within a single group (see internal/domain.GroupRole).
// 'read' (the default) may only view that group's members' clusters; 'write' may also manage them
// (scale, upgrade, delete, kubeconfig, shell). It never restricts a user's own clusters, and is moot
// for admins. The role is per-membership - a user can be Read in one group and Write in another.
export type GroupRole = 'read' | 'write';

// GroupMembership is one of a user's group memberships: the group and their role within it (see
// internal/domain.GroupMembership).
export interface GroupMembership {
  group_id: string;
  role: GroupRole;
}

// ResourceQuota mirrors domain.ResourceQuota: an amount of capacity on one infrastructure.
//
// disk_gb is storage - every node's root disk plus every extra NodeDisk. It is metered like vCPU and
// memory because a pool's root-disk override and a node's extra disks are two ways for a tenant to
// spend a host's storage pool without bound.
export interface ResourceQuota {
  vcpu: number;
  mem_mb: number;
  disk_gb: number;
}

export interface User {
  id: string;
  username: string;
  // Where this account's credential lives: 'local' (a password here) or 'ldap' (the directory).
  // Absent on rows that predate directory auth, which means local.
  auth_source?: string;
  // Directory-supplied and cosmetic - sAMAccountNames make the admin table hard to read without
  // them. Empty for local accounts.
  display_name?: string;
  email?: string;
  is_admin: boolean;
  // Quota is granted PER INFRASTRUCTURE, keyed by provider ("kvm", "vsphere") - capacity doesn't
  // move between backends, so a grant always names one. A missing key is no capacity there.
  // Admins hold no stored grant: their budget on each backend is its live unallocated pool.
  quotas?: Record<string, ResourceQuota> | null;
  memberships?: GroupMembership[] | null; // the groups this user belongs to (empty/absent = none)
  created_at: string;
}

// UserView is a user plus rolled-up usage, for the admin table (GET /users). `usage` breaks that
// usage down per infrastructure, to sit next to the per-infrastructure grants in `quotas`.
export interface UserView extends User {
  used_vcpu: number;
  used_mem_mb: number;
  used_disk_gb: number;
  cluster_count: number;
  usage?: Record<string, ResourceQuota> | null;
}

// ProviderAllocation is one infrastructure's conserved pool: its ceiling, how much has been granted
// out of it to tenants, and how much the admins' own live clusters already consume from the rest.
// Free to grant is total − allocated − admin_used; the granted total - not the summed platform
// total - is what a tenant grant is checked against.
export interface ProviderAllocation {
  provider: string;
  total_vcpu: number;
  total_mem_mb: number;
  total_disk_gb: number;
  allocated_vcpu: number;
  allocated_mem_mb: number;
  allocated_disk_gb: number;
  admin_used_vcpu: number;
  admin_used_mem_mb: number;
  admin_used_disk_gb: number;
}

// UsersReport is the admin dashboard payload: every user plus the allocation summary. `allocation`
// is the operative part - one conserved pool per infrastructure. total_*/allocated_* are their
// sums, a headline only: no grant is ever checked against them.
export interface UsersReport {
  users: UserView[];
  total_vcpu: number;
  total_mem_mb: number;
  total_disk_gb: number;
  allocated_vcpu: number;
  allocated_mem_mb: number;
  allocated_disk_gb: number;
  allocation: ProviderAllocation[];
  // shared_quota: per-user quota is off (KAAS_SHARED_QUOTA) - every account draws from each
  // backend's full ceiling. When true the Admin page hides the grant editor and shows each user's
  // consumption of the shared pool instead; allocated_* then reflects only dormant grants.
  shared_quota?: boolean;
}

// Group is a team (see internal/domain.Group). Members of the same group get access to each other's
// clusters - view-only or full, per each member's own role in the group.
export interface Group {
  id: string;
  name: string;
  // Who owns this group's roster: 'local' (admins manage it here) or 'ldap' (a directory mapping
  // rule owns it - the portal shows it read-only and membership is recomputed on every login).
  // Absent means local.
  source?: string;
  // Which directory rule owns it (the rule's group_key). Empty for local groups.
  source_key?: string;
  created_at: string;
}

// directoryManaged reports whether a directory rule owns this group, meaning its roster, name and
// existence are all config-driven and not editable here. The API rejects those edits regardless -
// this is only so the UI doesn't offer them.
export function directoryManaged(g: Pick<Group, 'source'>): boolean {
  return g.source === 'ldap';
}

// fromDirectory reports whether an account is authenticated by the directory rather than a local
// password.
export function fromDirectory(u: Pick<User, 'auth_source'>): boolean {
  return u.auth_source === 'ldap';
}

// AuthConfig is GET /auth/config: what the login page needs before it can authenticate. Public.
export interface AuthConfig {
  mode: 'local' | 'ldap';
  registration_enabled: boolean;
}

// GroupView is a group plus its members' usernames (GET /groups).
export interface GroupView extends Group {
  members: string[] | null;
  // A directory group whose mapping rule was removed from the config. Nothing syncs it any more, so
  // the portal lets an admin rename or delete it - unlike a live directory group. Removing a rule
  // deliberately leaves the group standing (a config typo must not destroy a team), so this is the
  // state where that gets cleaned up.
  orphaned?: boolean;
}

// directoryLocked reports whether a group is currently owned by a live mapping rule, and therefore
// must not be edited here: the roster comes from the directory, and a rename would be undone at the
// next boot. An ORPHANED directory group is editable - nothing will overwrite it.
export function directoryLocked(g: GroupView): boolean {
  return directoryManaged(g) && !g.orphaned;
}

// NodePool mirrors internal/domain.NodePool: a named, independently-scalable group of worker nodes,
// all at one t-shirt size. Every cluster is created with a "default" pool; afterwards pools are added
// and removed freely. `size` is immutable once the pool exists (the server rejects a change) - to
// move a pool to a different size, add one and drain the old away.
export interface NodePool {
  name: string;
  size: string;
  desired_workers: number;
  // Root-disk size of this pool's workers, in GB. Absent/0 means the t-shirt size's default (50).
  // It only ever grows the default, and - like `size` - it is IMMUTABLE once the pool exists: the
  // server rejects a change, because growing it would rebuild every node in the pool. To add storage
  // to a running node, attach a NodeDisk instead.
  disk_gb?: number;
}

// The mount point Longhorn takes as its data path, and so the mount point of the platform's own
// per-worker storage disk. A disk mounted here (or at a "<path>-<name>" sibling) is pool capacity;
// a disk mounted anywhere else is an ordinary filesystem Longhorn ignores. Mirrors
// domain.LonghornDataPath / domain.LonghornMountPath.
export const LONGHORN_DATA_PATH = '/var/lib/longhorn';
export const PLATFORM_STORAGE_DISK = 'storage';
export function longhornMountPath(diskName: string): string {
  return diskName === PLATFORM_STORAGE_DISK ? LONGHORN_DATA_PATH : `${LONGHORN_DATA_PATH}-${diskName}`;
}
export function feedsStoragePool(d: NodeDisk): boolean {
  return d.mount_path === LONGHORN_DATA_PATH || d.mount_path.startsWith(`${LONGHORN_DATA_PATH}-`);
}
// The platform's own per-worker disk is derived from the cluster's storage_disk_gb, so the API
// refuses to delete it individually - the portal must not offer to.
export function isPlatformStorageDisk(d: NodeDisk): boolean {
  return d.name === PLATFORM_STORAGE_DISK;
}

// NodeDisk mirrors internal/domain.NodeDisk: an EXTRA block device attached to one worker node
// beyond its root disk, formatted with LVM and mounted at `mount_path`.
//
// Keyed on the node's VM NAME rather than a node id, because a node row is observed state and is
// re-created whenever its VM is (a rolling OS replacement mints a new one) - the VM name is what's
// stable, so a node rebuilt underneath keeps its disks.
export interface NodeDisk {
  vm_name: string;
  name: string; // logical name, unique per node; also names the LVM volume group
  size_gb: number;
  mount_path: string;
  fs_type: 'ext4' | 'xfs';
  // pending  - requested; the volume may not exist yet and is certainly not mounted.
  // attached - created, attached and mounted.
  // removing - being torn down: unmounted in the guest first, then detached and destroyed.
  phase: 'pending' | 'attached' | 'removing';
  wwn?: string;
  device_id?: string; // observed guest identity; empty until the disk actually exists
}

// The filesystems an extra disk may be formatted with (domain.FSExt4 / domain.FSXFS).
export const DISK_FS_TYPES = ['ext4', 'xfs'] as const;

// Bounds mirroring domain.MinDiskGB / MaxDiskGB and MaxRootDiskGB. Client-side these only shape the
// inputs; the server is the authoritative gate.
export const MIN_DISK_GB = 1;
export const MAX_DISK_GB = 2000;
export const MAX_ROOT_DISK_GB = 2000;

// The add-on that turns the per-worker storage disks into the cluster's default StorageClass, and
// the disk size a cluster gets when the wizard is left alone (domain.DefaultStorageDiskGB).
export const LONGHORN_ADDON = 'longhorn';
export const DEFAULT_STORAGE_DISK_GB = 10;

// Mount paths an extra disk may not use - a fresh filesystem mounted on one of these hides the
// running system underneath it. Mirrors domain.protectedMounts; the server rejects these regardless.
export const PROTECTED_MOUNTS = [
  '/', '/boot', '/etc', '/usr', '/bin', '/sbin', '/lib', '/lib64', '/dev', '/proc', '/sys', '/run',
  '/var', '/var/lib', '/var/lib/kubelet', '/var/lib/containerd', '/var/lib/etcd', '/etc/kubernetes',
  '/home', '/root', '/tmp',
];

export interface Cluster {
  id: string;
  name: string;
  owner_id: string; // the User that owns this cluster
  owner_username: string; // resolved server-side so viewers don't need admin-only /users access
  can_manage: boolean; // whether the current user may mutate this cluster (server-computed per-actor)
  size: string; // t-shirt size of the CONTROL-PLANE nodes; workers are sized per node pool
  node_pools: NodePool[]; // the cluster's worker pools; total worker count is workerCount() over these
  node_disks?: NodeDisk[]; // extra block devices attached to individual worker nodes
  // Size of the per-worker storage disk backing the cluster's default (Longhorn) StorageClass. Set at
  // creation and immutable; 0 = the cluster provisions none.
  storage_disk_gb?: number;
  provider: string; // infrastructure the cluster runs on: 'kvm' | 'vsphere' | 'proxmox' (see ProviderInfo)
  network_cidr: string; // kvm: the cluster's own libvirt NAT network. vsphere/proxmox: the shared operator subnet
  pod_cidr: string;
  svc_cidr: string;

  // Shared-network providers (vsphere, proxmox) only: the operator network (portgroup / bridge), how
  // node IPs are assigned, and (static mode) the gateway/DNS and the platform's per-node allocation.
  network_name?: string;
  ip_mode?: 'dhcp' | 'static';
  net_gateway?: string;
  net_dns?: string;
  static_ips?: Record<string, string>;

  control_planes: number;
  api_vip?: string;
  load_balancer_ip?: string; // reserved address for the default MetalLB pool / Envoy Gateway
  dns_domain?: string; // the cluster's own subdomain, e.g. dev.kaas.example.internal
  apps_domain?: string; // apps are published under *.<apps_domain> → load_balancer_ip
  vault_wired?: boolean; // the cluster's Vault path + policies + ESO wiring have been provisioned

  bundle: string;
  os_image: string;
  k8s_version: string;
  cni: string;
  cni_version: string;
  target_bundle?: string; // bundle an in-progress upgrade is promoting toward (empty when idle)

  phase: Phase;
  status: string;

  generation: number;
  observed_generation: number;

  nodes: Node[] | null;
  addons: Addon[] | null;

  // Observed state of the cluster's etcd backend store, re-read by the reconciler on a cadence and
  // the trigger for automatic defragmentation. Absent until first observed.
  etcd?: EtcdStatus;

  // When the platform last stored a control-plane backup for this cluster. Absent means never - the
  // state in which a dead sole control plane is not recoverable. The backup itself is never served
  // to the browser: it holds every Secret in the cluster in plaintext plus the CA key.
  etcd_snapshot_at?: string;

  // What automatic repair currently believes about this cluster's nodes. Absent until first observed.
  repair?: ClusterRepair;

  created_at: string;
  deleted_at?: string | null;
}

// EtcdStatus is the observed size, fragmentation and alarm state of a cluster's etcd backend store
// (see internal/domain.EtcdStatus). db_bytes is the physical bbolt file; db_in_use_bytes the part of
// it actually holding data - the gap is fragmentation, which only defragmentation reclaims.
export interface EtcdStatus {
  db_bytes: number;
  db_in_use_bytes: number;
  quota_bytes?: number; // configured --quota-backend-bytes; absent = etcd's 2GiB default
  alarms?: string[]; // NOSPACE (cluster is read-only) | CORRUPT
  members: number; // members that answered the last status read
  observed_at: string;
  defragged_at?: string;
}

// ClusterRepair is what automatic repair believes about a cluster (see internal/domain.ClusterRepair).
// `nodes` is keyed by VM name; `target`/`action` name the repair currently in flight, if any.
export interface ClusterRepair {
  nodes?: Record<string, NodeRepairState>;
  target?: string;
  action?: RepairAction;
  observed_at?: string;
}

// One rung of the repair escalation ladder, cheapest first.
export type RepairAction = 'power-on' | 'rejoin' | 'restart-kubelet' | 'replace' | 'restore';

// What the platform believes is wrong with one node.
export type NodeFault = 'notready' | 'missing' | 'vm-down';

// NodeRepairState is the platform's durable memory about one node: how long it has been faulty, what
// has been tried, and whether repair has given up on it. `unhealthy_since` is the field the whole
// feature turns on - a health snapshot says a node is NotReady, only this says for how long.
export interface NodeRepairState {
  fault?: NodeFault;
  unhealthy_since?: string;
  attempts?: number;
  last_action?: RepairAction;
  last_action_at?: string;
  repaired_at?: string;
  // The platform has stopped trying. Self-healing has ended here and a human is needed.
  suspended?: boolean;
  note?: string;
}

// CapacityReport is the actor's own quota (GET /capacity). `providers` is the real unit - one
// entry per infrastructure, with the grant and the usage there; a cluster is admitted against its
// own infrastructure's entry, never the sum. total_*/used_* are the sums, for headline figures.
export interface CapacityReport {
  total_vcpu: number;
  total_mem_mb: number;
  total_disk_gb: number;
  used_vcpu: number;
  used_mem_mb: number;
  used_disk_gb: number;
  providers?: ProviderQuota[] | null;
}

// ProviderQuota is one infrastructure's slice of an account's quota: granted there, used there.
export interface ProviderQuota {
  provider: string;
  total_vcpu: number;
  total_mem_mb: number;
  total_disk_gb: number;
  used_vcpu: number;
  used_mem_mb: number;
  used_disk_gb: number;
}

// ---- profile (GET /auth/profile) ----
// The actor's own account view. It exists because /auth/me's memberships carry only a group ID and
// the endpoint that resolves those to names is admin-only - this resolves the caller's OWN groups
// and nothing else. `capacity` is the same payload as GET /capacity.

// ProfileGroup is one of the actor's memberships with the group's name resolved.
export interface ProfileGroup {
  id: string;
  name: string;
  source?: string; // 'local' | 'ldap' - see Group.source
  role: GroupRole;
}

export interface ProfileReport {
  user: User;
  groups?: ProfileGroup[] | null;
  capacity: CapacityReport;
}

// ---- metrics (GET /clusters/{id}/metrics) ----
// The latest live resource-usage snapshot, sampled worker-side from the in-cluster metrics API
// (metrics-server) and served read-through. CPU in millicores, memory in bytes - the units the
// Kubernetes metrics API uses. Cluster-wide totals are summed from the per-node samples.
// A 204 (null here) means Ready but no snapshot yet, or metrics-server disabled.

export interface NodeMetrics {
  node_name: string;
  cpu_used_milli: number;
  cpu_capacity_milli: number;
  mem_used_bytes: number;
  mem_capacity_bytes: number;
}

export interface MetricsSnapshot {
  cluster_id: string;
  collected_at: string;
  nodes: NodeMetrics[];
}

// ---- health (GET /clusters/{id}/health) ----
// The latest cluster-health snapshot, evaluated worker-side against the cluster API (nodes Ready,
// system workloads, scheduling capacity, etcd quorum, add-on availability) and served read-through.
// `status` is the rolled-up worst-of the checks; 'unknown' checks (not applicable / not evaluable)
// don't count against it. A 204 (null here) means Ready but no snapshot yet, or health disabled.

export type HealthStatus = 'healthy' | 'degraded' | 'unhealthy' | 'unknown';

export interface HealthCheck {
  // Stable slug: api-server | nodes-ready | system-workloads | scheduling-capacity | etcd-quorum |
  // etcd-store | addon-availability | control-plane-certs | etcd-backup | auto-repair. The last four
  // are derived from stored control-plane state rather than probed, so they read the same whether
  // the platform is running against real clusters or fakes.
  id: string;
  name: string;
  status: HealthStatus;
  summary: string;
}

export interface NodeHealth {
  node_name: string;
  ready: boolean;
  cordoned?: boolean; // spec.unschedulable - set by `kubectl cordon`/drain; doesn't affect `ready`
  pressures?: string[]; // active node pressure conditions (MemoryPressure/DiskPressure/PIDPressure)
}

export interface HealthSnapshot {
  cluster_id: string;
  status: HealthStatus;
  checks: HealthCheck[];
  nodes: NodeHealth[];
  collected_at: string;
}

export interface ClusterEvent {
  cluster_id: string;
  ts: string;
  level: 'info' | 'warn' | 'error';
  source: 'infra' | 'ansible' | 'addon' | 'reconciler' | string;
  message: string;
}

// ---- operations (GET /clusters/{id}/operations) ----
// The per-cluster action history: one entry per user intent (create / scale / add-ons / upgrade),
// higher-level than a ClusterEvent. Recorded when desired state is written; flipped to
// 'completed' (with finished_at) once the reconciler converges the cluster. See internal/domain.

// User-initiated kinds (create…ssh) plus the platform-initiated maintenance/repair the reconciler
// records on its own - see internal/domain OperationKind. 'disks' is a user-initiated node-disk edit.
export type OperationKind =
  | 'create'
  | 'scale'
  | 'addons'
  | 'upgrade'
  | 'disks'
  | 'ssh'
  | 'repair'
  | 'restore'
  | 'snapshot'
  | 'cert-renewal'
  | 'defrag';
export type OperationStatus = 'in_progress' | 'completed';

export interface Operation {
  id: string;
  cluster_id: string;
  kind: OperationKind;
  summary: string;
  detail?: string;
  generation: number;
  status: OperationStatus;
  actor_username?: string; // who triggered this operation (denormalized; may be absent for old records)
  started_at: string;
  finished_at?: string | null;
}

// ---- catalog (GET /catalog) ----

export type CatalogStatus = 'supported' | 'deprecated' | 'eol';

export interface OSImage {
  name: string;
  family: string;
  release: string;
  status: CatalogStatus;
  baseImageURL: string;
  goldenImage: string;
}

export interface K8sVersion {
  version: string;
  status: CatalogStatus;
}

export interface CatalogAddon {
  name: string;
  type: 'cni' | 'addon';
  version: string;
  status: CatalogStatus;
  repo: string;
  chart: string;
  description?: string;
  values?: Record<string, string>;
}

export interface Bundle {
  name: string;
  status: CatalogStatus;
  os: string;
  kubernetes: string;
  cni: string;
  addons: Record<string, string>;
  supersedes: string;
}

export interface Catalog {
  os: OSImage[];
  kubernetes: K8sVersion[];
  addons: CatalogAddon[];
  bundles: Bundle[];
  // The infrastructure providers this deployment offers (KAAS_INFRA_PROVIDERS, in order - the
  // first is the default). The wizard shows an Infrastructure step only when there's more than
  // one; with a single provider the choice is implicit.
  providers?: ProviderInfo[];
}

// ProviderInfo mirrors app.ProviderInfo: an enabled infrastructure provider, plus (for a shared-
// network provider - vsphere, proxmox) the deployment's network shape - which tells the wizard where
// nodes will land and whether the user must supply an HA control-plane VIP (dhcp mode) or the
// platform allocates one (static).
export interface ProviderInfo {
  name: string;
  ip_mode?: 'dhcp' | 'static';
  network_name?: string;
  network_cidr?: string;
  net_range?: string; // static ip_mode only: the "from-to" range node addresses + the HA VIP are allocated from
}

// ---- custom catalogs (per-user add-on catalogs) ----
// A user-owned collection of self-defined Helm-chart add-ons, shared through the group model exactly
// like clusters (see internal/domain.CustomCatalog). Each add-on is a chart definition + values.

export interface CustomAddon {
  name: string;
  description?: string;
  repo?: string; // classic HTTP chart-repo URL (empty for an oci:// chart)
  chart: string; // oci:// ref or chart name
  version: string;
  namespace?: string; // install namespace (empty = same as name)
  values?: string; // full Helm values YAML (empty = chart defaults)
}

// CustomCatalogView is a catalog plus the actor's access level and the owner's username, as returned
// by the list/get endpoints. access 'edit' = may modify; 'view' = read-only (a read-role group-mate).
export interface CustomCatalogView {
  id: string;
  owner_id: string;
  name: string;
  addons: CustomAddon[] | null;
  created_at: string;
  owner_username: string;
  access: 'view' | 'edit';
}

// A (catalog, add-on) selection for cluster create/update.
export interface CustomAddonRef {
  catalog_id: string;
  name: string;
}

// ---- request bodies ----

export interface CreateClusterRequest {
  name: string;
  size: string; // control-plane node size; workers are sized per pool
  // The worker pools to create. The server always ensures a pool named "default" exists, so omitting
  // this yields one empty default pool.
  node_pools?: NodePool[];
  ha: boolean;
  bundle?: string;
  addons?: string[];
  addon_values?: Record<string, string>; // add-on name -> edited Helm values YAML (from the editor)
  custom_addons?: CustomAddonRef[]; // add-ons picked from the user's custom catalogs
  network_cidr?: string; // kvm only; omit/empty = server auto-allocates a free block
  provider?: string; // omit = the deployment's default provider
  api_vip?: string; // required for an HA control plane on vsphere in dhcp mode
  load_balancer_ip?: string; // required on vsphere/proxmox in dhcp mode: the default MetalLB pool / Envoy Gateway address
  // storage_disk_gb sizes the extra disk every worker is born with, which backs the cluster's default
  // (Longhorn) StorageClass. Omit for the platform default (10); 0 provisions none. Immutable after
  // creation - capacity is grown by attaching more disks.
  storage_disk_gb?: number;
}

export interface UpdateClusterRequest {
  // The desired WHOLE pool list, replacing what's there (same declarative shape as `addons`).
  // undefined = leave the pools untouched. Scaling, adding and removing a pool are all this one edit.
  node_pools?: NodePool[];
  addons?: string[];
  addon_values?: Record<string, string>; // starting Helm values for add-ons being ADDED in this edit
  custom_addons?: CustomAddonRef[]; // null/undefined = leave existing custom add-ons untouched
}

// AddNodeDiskRequest attaches a new extra disk to one worker node (POST /clusters/{id}/disks).
// The cluster must be Ready. fs_type is optional and defaults to ext4 server-side.
export interface AddNodeDiskRequest {
  vm_name: string;
  name: string;
  size_gb: number;
  // Omit to feed the cluster's storage pool: the server mounts it at longhornMountPath(name) and
  // registers it with Longhorn. Naming another path gives an ordinary filesystem instead.
  mount_path?: string;
  fs_type?: 'ext4' | 'xfs';
}

// Admin edit to an account: quota, group memberships, or both. `quotas` is a MERGE keyed by
// infrastructure - only the providers present are changed, so one backend can be topped up without
// restating the others. A non-undefined `memberships` replaces the user's entire membership set
// ([] removes them from every group).
export interface UpdateUserRequest {
  quotas?: Record<string, ResourceQuota>;
  memberships?: GroupMembership[];
}

// t-shirt sizes mirror internal/domain.Sizes so the wizard can preview capacity locally.
export interface SizeSpec {
  cpus: number;
  memMB: number;
  diskGB: number;
}

// NOTE: diskGB is the ROOT disk and is deliberately the same for every size - a bigger node is a
// bigger CPU/memory shape, not more storage. Storage is sized separately: a pool can override its
// workers' root disk (NodePool.disk_gb), and a node can carry extra disks (NodeDisk). These values
// must match domain.Sizes exactly; the wizard prices a cluster from them.
export const SIZES: Record<string, SizeSpec> = {
  small: { cpus: 2, memMB: 8192, diskGB: 50 },
  medium: { cpus: 4, memMB: 16384, diskGB: 50 },
  large: { cpus: 8, memMB: 32768, diskGB: 50 },
};

// ---- workloads (mirrors internal/kube; the Workloads page) ----
// The request-driven kube query seam - live workloads inside a Ready cluster. Only Deployments and
// StatefulSets are scalable by replicas (see WorkloadKind.Scalable server-side).

export type WorkloadKind = 'deployment' | 'statefulset' | 'daemonset' | 'job' | 'cronjob';

export const WORKLOAD_KINDS: WorkloadKind[] = [
  'deployment',
  'statefulset',
  'daemonset',
  'job',
  'cronjob',
];

export interface WorkloadSummary {
  kind: WorkloadKind;
  namespace: string;
  name: string;
  ready_replicas: number;
  desired_replicas: number;
  status: string;
  images: string[] | null;
  created_at: string;
  schedule?: string; // CronJob
  suspended?: boolean; // CronJob
}

export interface Container {
  name: string;
  image: string;
}

export interface WorkloadCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  updated?: string;
}

export interface PodInfo {
  name: string;
  ready: string; // "1/1"
  status: string;
  restarts: number;
  node: string;
  ip: string;
  created_at: string;
  containers: string[] | null;
}

export interface WorkloadDetail extends WorkloadSummary {
  updated_replicas: number;
  available_replicas: number;
  strategy?: string;
  selector?: Record<string, string>;
  labels?: Record<string, string>;
  containers: Container[] | null;
  conditions: WorkloadCondition[] | null;
  pods: PodInfo[] | null;
}

export interface WorkloadEvent {
  type: string; // Normal | Warning
  reason: string;
  message: string;
  count: number;
  last_seen: string;
  object: string;
}

// ---- storage (mirrors internal/kube/storage.go; the Storage page) ----
// The same kube query seam, storage half: a Ready cluster's PersistentVolumeClaims and
// StorageClasses. Read-only - there is no storage mutation in the portal.

// PVCStatus is a claim's binding phase, as the API server reports it.
export type PVCStatus = 'Pending' | 'Bound' | 'Lost';

export interface PVCSummary {
  namespace: string;
  name: string;
  status: PVCStatus | string;
  volume?: string; // bound PV name ("" while Pending)
  capacity?: string; // actual bound capacity, e.g. "8Gi"
  requested?: string; // what the claim asked for
  access_modes: string[] | null; // RWO | ROX | RWX | RWOP
  storage_class: string;
  volume_mode?: string; // Filesystem | Block
  created_at: string;
}

// PersistentVolume is the bound PV's properties, shown on a claim's Overview.
export interface PersistentVolume {
  name: string;
  capacity?: string;
  status?: string;
  reclaim_policy?: string;
  storage_class?: string;
  source?: string; // short rendering of the backing driver, e.g. "csi: ebs.csi.aws.com"
  created_at: string;
}

export interface PVCDetail extends PVCSummary {
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  conditions: WorkloadCondition[] | null;
  used_by: string[] | null; // pods currently mounting the claim
  // The bound PV object, absent while unbound. Named persistent_volume, not volume: PVCSummary.volume
  // is already the PV's *name* (mirrors internal/kube.PVCDetail).
  persistent_volume?: PersistentVolume;
}

// StorageClass carries every field its detail view shows - the list is the detail (only the YAML is
// fetched on demand), mirroring internal/kube.StorageClass.
export interface StorageClass {
  name: string;
  provisioner: string;
  reclaim_policy?: string;
  volume_binding_mode?: string; // Immediate | WaitForFirstConsumer
  allow_expansion: boolean;
  is_default: boolean;
  parameters?: Record<string, string>;
  mount_options?: string[];
  labels?: Record<string, string>;
  created_at: string;
}

// ---- config & secrets (mirrors internal/kube/config.go; the Secrets page) ----
// ConfigMap values are returned in full - a ConfigMap is not a secret. Secret VALUES are never
// present: a summary carries key names, a detail carries key + byte length only (redacted server-side).

export interface ConfigMapSummary {
  namespace: string;
  name: string;
  keys: string[] | null;
  data_count: number;
  immutable?: boolean;
  created_at: string;
}

export interface ConfigMapDetail extends ConfigMapSummary {
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  data?: Record<string, string>; // full textual values (ConfigMaps are not secret)
  binary_keys?: string[]; // binaryData keys, whose values are omitted
}

export interface SecretSummary {
  namespace: string;
  name: string;
  type: string; // Opaque | kubernetes.io/tls | ...
  keys: string[] | null; // key NAMES only, never values
  data_count: number;
  immutable?: boolean;
  managed_by?: string; // owning ExternalSecret when Vault-synced by ESO, else absent
  created_at: string;
}

// SecretKeyInfo is one key of a Secret with its value REDACTED - only the byte length is revealed.
export interface SecretKeyInfo {
  key: string;
  bytes: number;
}

export interface SecretDetail extends SecretSummary {
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  key_info: SecretKeyInfo[] | null; // per-key metadata, values redacted
}

// VaultSession is the "View in Vault" handoff: GET /clusters/{id}/vault-session mints a short-lived,
// access-scoped Vault token and returns the UI URL for the cluster's path.
export interface VaultSession {
  url: string;
  token: string;
}

// ---- networking (mirrors internal/kube/network.go; the Networking page) ----
// The same kube query seam, network half: a Ready cluster's Services and Gateway API Gateways/Routes,
// plus the platform's own north-south contract (the reserved gateway address and the wildcard DNS
// record every cluster gets). Read-only throughout.

// RouteKind is a Gateway API route kind. Mirrors internal/kube.RouteKind - the lowercase wire form.
export type RouteKind = 'httproute' | 'grpcroute' | 'tcproute' | 'tlsroute' | 'udproute';

// ObjectRef identifies a namespaced networking object (a Service, Gateway or Route).
export interface ObjectRef {
  namespace: string;
  name: string;
}

export interface ServicePort {
  name?: string;
  protocol?: string; // TCP | UDP | SCTP
  port: number;
  target_port?: string; // may be a named port, so a string
  node_port?: number;
  app_protocol?: string;
}

export interface ServiceSummary {
  namespace: string;
  name: string;
  type: string; // ClusterIP | NodePort | LoadBalancer | ExternalName
  cluster_ip?: string;
  external_ips?: string[] | null;
  ports: ServicePort[] | null;
  selector?: Record<string, string>;
  endpoints: number; // ready backing endpoints
  created_at: string;
}

export interface ServiceBackend {
  pod?: string;
  ip: string;
  node?: string;
}

export interface ServiceDetail extends ServiceSummary {
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  session_affinity?: string;
  external_traffic_policy?: string;
  ip_families?: string[] | null;
  external_name?: string;
  backends: ServiceBackend[] | null;
}

export interface GatewayListener {
  name: string;
  protocol: string; // HTTP | HTTPS | TCP | TLS | UDP
  port: number;
  hostname?: string;
  tls_mode?: string; // Terminate | Passthrough; "" = plaintext
  certificate_refs?: string[] | null;
  attached_routes: number;
  programmed: boolean;
  status?: string; // a reason when not programmed
}

// GatewaySummary carries everything the Gateway drawer shows besides its YAML - the list is the
// detail. is_default marks the platform's own Gateway (the one tenants attach routes to).
export interface GatewaySummary {
  namespace: string;
  name: string;
  class: string;
  addresses?: string[] | null;
  listeners: GatewayListener[] | null;
  programmed: boolean;
  status?: string;
  is_default?: boolean;
  labels?: Record<string, string>;
  created_at: string;
}

export interface ParentRef {
  namespace: string;
  name: string;
  section_name?: string; // the pinned listener, when any
  port?: number;
  accepted: boolean;
  status?: string;
}

export interface RouteBackend {
  namespace?: string;
  name: string;
  kind?: string; // Service unless the route says otherwise
  port?: number;
  weight?: number;
}

export interface RouteRule {
  matches?: string[] | null; // short human forms ("PathPrefix: /api")
  backends: RouteBackend[] | null;
}

export interface RouteSummary {
  kind: RouteKind;
  namespace: string;
  name: string;
  hostnames?: string[] | null;
  parent_refs: ParentRef[] | null;
  rules: RouteRule[] | null;
  accepted: boolean;
  status?: string;
  labels?: Record<string, string>;
  created_at: string;
}

// ExposedApp is one externally reachable application - a route hostname attached to a Gateway, with
// the URL it answers on. Derived server-side (internal/kube.ExposedApps) so every client agrees on
// what "exposed" means. platform_domain reports that the hostname is covered by the cluster's own
// wildcard DNS record, i.e. it needs no DNS of the user's own.
export interface ExposedApp {
  hostname: string;
  url: string;
  tls: boolean;
  route: ObjectRef;
  route_kind: RouteKind;
  gateway: ObjectRef;
  address?: string;
  backends: RouteBackend[] | null;
  platform_domain: boolean;
  accepted: boolean;
  status?: string;
}

// NetworkAddons reports which of the north-south add-ons are installed on the cluster, so a page that
// is empty because the user deselected envoy-gateway can say so.
export interface NetworkAddons {
  gateway: boolean; // envoy-gateway
  metallb: boolean;
  cert_manager: boolean;
  external_dns: boolean;
}

// NetworkOverview is the Networking page's headline: the cluster's default north-south contract plus
// what is actually published through it. Mirrors internal/kube.NetworkOverview.
export interface NetworkOverview {
  load_balancer_ip?: string;
  apps_domain?: string;
  dns_domain?: string;
  wildcard_record?: string; // rendered as it appears in the zone
  gateway_wired: boolean;
  dns_wired: boolean;
  addons: NetworkAddons;
  default_gateway?: GatewaySummary;
  exposed_apps: ExposedApp[] | null;
  load_balancer_services: ServiceSummary[] | null;
  service_count: number;
  route_count: number;
  gateway_count: number;
}

// ---- monitoring (the Monitoring page; PromQL query seam, see internal/monitoring) ----

// PanelKind determines how a panel renders. Mirrors internal/monitoring.PanelKind.
export type PanelKind = 'slo' | 'gauge' | 'stat' | 'timeseries' | 'bars' | 'status' | 'alerts';

// PanelViz refines how a timeseries panel draws its series. Mirrors internal/monitoring.Viz*.
export type PanelViz = '' | 'area' | 'stacked';

// Unit tags how a value/axis is formatted. Mirrors the Unit strings in the panel registry.
export type PanelUnit =
  | ''
  | 'ratio' // 0..1 → percent
  | 'count'
  | 'cores'
  | 'rps'
  | 'ops'
  | 'pps'
  | 'bytes'
  | 'Bps'
  | 'ms'
  | 's';

export interface MonitoringPoint {
  t: number; // unix seconds
  v: number;
}

export interface MonitoringSeries {
  name: string;
  points: MonitoringPoint[];
}

export interface MonitoringStatusRow {
  label: string;
  up: boolean;
  detail?: string;
}

export interface MonitoringAlert {
  name: string;
  severity: string; // critical | warning | info | none
  state: string; // firing | pending
  summary: string;
  active_at?: string;
}

// MonitoringBar is one row of a top-k bars panel (already sorted largest-first by the server).
export interface MonitoringBar {
  name: string;
  value: number;
}

export interface PanelResult {
  id: string;
  title: string;
  unit: PanelUnit;
  kind: PanelKind;
  value?: number; // slo | gauge (0..1) | stat (raw)
  target?: number; // slo target (0..1)
  series?: MonitoringSeries[]; // timeseries; also a stat's sparkline when present
  bars?: MonitoringBar[];
  rows?: MonitoringStatusRow[];
  alerts?: MonitoringAlert[];
  error?: string;
  // Layout/presentation hints, orthogonal to kind (mirrors internal/monitoring.PanelResult).
  section?: string; // titled group the panel renders under on its tab
  desc?: string; // one-line explanation shown as an info tooltip
  viz?: PanelViz; // timeseries only
  featured?: boolean; // render bigger and stand out (cluster CPU/memory on Overview)
}

export interface MonitoringTabData {
  tab: string;
  generated_at: string;
  panels: PanelResult[];
}

export interface MonitoringTab {
  id: string;
  title: string;
}

// MonitoringApp is one in-cluster web UI the "Open UI" links point at, reverse-proxied per cluster
// at /api/clusters/{id}/proxy/{id}/. Mirrors tunnel.App. Despite the name it covers every surface -
// the Monitoring page's Grafana/Prometheus/Alertmanager and the Storage page's Longhorn.
export interface MonitoringApp {
  id: string; // url segment: grafana | prometheus | alertmanager | longhorn
  name: string; // display label
  // write_scoped: the app's own UI can mutate cluster state and it has no user model to express our
  // read/write split (Alertmanager silences alerts), so it is gated on write access. The link is
  // hidden for actors without can_manage; the API is the authoritative gate (403).
  write_scoped: boolean;
}

// MonitoringMeta is the tab bar + gating flags (GET /clusters/{id}/monitoring).
export interface MonitoringMeta {
  enabled: boolean;
  ready: boolean;
  tabs: MonitoringTab[];
  ranges: string[]; // selectable time-range picker windows ("5m"…"12h")
  default_range: string; // the window the page opens with
  apps: MonitoringApp[]; // in-cluster UIs the "Open UI" links open in a new tab
}

// StorageAppsMeta is the Storage page's "Open UI" gating (GET /clusters/{id}/storage/apps): the
// Longhorn UI link, and whether this cluster can serve it at all.
export interface StorageAppsMeta {
  enabled: boolean; // the longhorn add-on is installed
  ready: boolean;
  apps: MonitoringApp[];
}

// ---- security (the Security page; Trivy CRD query seam, see internal/security) ----

// SecurityKind is one of the four Trivy report families. Mirrors internal/security.Kind.
export type SecurityKind = 'vulnerability' | 'configaudit' | 'exposedsecret' | 'rbacassessment';

// Severity is a normalized Trivy severity, most-severe first. Mirrors internal/security.Severity.
export type Severity = 'critical' | 'high' | 'medium' | 'low' | 'unknown';

// SecurityCounts is a per-severity breakdown, the common currency of every security summary.
export interface SecurityCounts {
  critical: number;
  high: number;
  medium: number;
  low: number;
  unknown: number;
}

// SecurityKindMeta describes one report family for the tab bar. Mirrors internal/security.KindMeta.
export interface SecurityKindMeta {
  id: SecurityKind;
  title: string;
  finding_noun: string;
  has_artifact: boolean;
  description: string;
}

// SecurityMeta is the tab bar + gating flags (GET /clusters/{id}/security).
export interface SecurityMeta {
  enabled: boolean;
  ready: boolean;
  kinds: SecurityKindMeta[];
}

export interface SecurityResource {
  kind: string; // Deployment, ReplicaSet, Role, …
  name: string;
  container?: string;
}

export interface SecurityArtifact {
  registry?: string;
  repository: string;
  tag?: string;
  digest?: string;
}

// SecurityReport is one Trivy report CR summarized (no findings). Mirrors internal/security.Report.
export interface SecurityReport {
  kind: SecurityKind;
  name: string;
  namespace: string;
  resource: SecurityResource;
  artifact?: SecurityArtifact; // vulnerability/exposedsecret only
  summary: SecurityCounts;
  scanner?: string;
  updated_at?: string;
}

// SecurityFinding is one entry inside a report. Only fields relevant to the kind are populated.
export interface SecurityFinding {
  id: string; // CVE / checkID / ruleID
  severity: Severity;
  title: string;
  // vulnerability
  resource?: string; // vulnerable package
  installed_version?: string;
  fixed_version?: string;
  score?: number;
  link?: string;
  // configaudit / rbacassessment
  category?: string;
  description?: string;
  remediation?: string;
  // exposedsecret
  match?: string;
  target?: string;
}

export interface SecurityReportDetail extends SecurityReport {
  findings: SecurityFinding[] | null;
}

export interface SecurityKindStat {
  kind: SecurityKind;
  title: string;
  report_count: number;
  totals: SecurityCounts;
}

export interface SecurityImageRisk {
  image: string;
  summary: SecurityCounts;
  workloads?: string[] | null;
}

export interface SecurityNamespaceRisk {
  namespace: string;
  totals: SecurityCounts;
}

// SecurityOverview is the cluster-wide dashboard (GET /clusters/{id}/security/overview).
export interface SecurityOverview {
  generated_at: string;
  kinds: SecurityKindStat[] | null;
  top_images: SecurityImageRisk[] | null;
  namespaces: SecurityNamespaceRisk[] | null;
}

// ---- audit (the Audit tab; API-server audit query seam, see internal/audit) ----

// AuditResource is the Kubernetes object an event acted on. Mirrors internal/audit.Resource.
export interface AuditResource {
  api_group?: string;
  resource?: string;
  subresource?: string;
  namespace?: string;
  name?: string;
}

// AuditEvent is one API-server audit record. Mirrors internal/audit.Event.
export interface AuditEvent {
  audit_id: string;
  timestamp: string;
  stage?: string;
  level?: string;
  verb: string;
  user: string;
  groups?: string[] | null;
  source_ips?: string[] | null;
  user_agent?: string;
  resource: AuditResource;
  request_uri?: string;
  response_code?: number;
}

export interface AuditVerbCount {
  verb: string;
  count: number;
}

// AuditStats is the page rollup shown as stat tiles. Mirrors internal/audit.Stats.
export interface AuditStats {
  total: number;
  denied: number;
  users: number;
  namespaces: number;
  by_verb: AuditVerbCount[] | null;
}

// AuditPage is one Audit-tab response (GET /clusters/{id}/audit). Mirrors internal/audit.Page.
export interface AuditPage {
  events: AuditEvent[] | null;
  stats: AuditStats;
  generated_at: string;
  truncated: boolean;
}

// AuditQuery is the filter set the Audit tab sends as query params.
export interface AuditQuery {
  limit?: number;
  verb?: string;
  namespace?: string;
  user?: string;
  resource?: string;
  q?: string;
}
