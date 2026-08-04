// Typed fetch wrapper for the control-plane API. Everything is same-origin under `/api`
// (nginx in prod, Vite proxy in dev). Errors surface the API's `{ "error": "..." }` body.

import type {
  AuthConfig,
  Catalog,
  CapacityReport,
  Cluster,
  CreateClusterRequest,
  UpdateClusterRequest,
  AddNodeDiskRequest,
  Bundle,
  Operation,
  MetricsSnapshot,
  HealthSnapshot,
  User,
  ProfileReport,
  UsersReport,
  UpdateUserRequest,
  Group,
  GroupView,
  WorkloadKind,
  WorkloadSummary,
  WorkloadDetail,
  WorkloadEvent,
  MonitoringMeta,
  StorageAppsMeta,
  MonitoringTabData,
  SecurityKind,
  SecurityMeta,
  SecurityOverview,
  SecurityReport,
  SecurityReportDetail,
  AuditPage,
  AuditQuery,
  PVCSummary,
  PVCDetail,
  StorageClass,
  ConfigMapSummary,
  ConfigMapDetail,
  SecretSummary,
  SecretDetail,
  VaultSession,
  ObjectRef,
  RouteKind,
  ServiceSummary,
  ServiceDetail,
  GatewaySummary,
  RouteSummary,
  NetworkOverview,
  AddonValuesView,
  CustomCatalogView,
  CustomAddon,
  VersionInfo,
} from './types';

// WorkloadRef identifies a workload for the detail/log/scale endpoints.
export interface WorkloadRef {
  kind: WorkloadKind;
  namespace: string;
  name: string;
}

const wlPath = (id: string, ref: WorkloadRef) =>
  `/clusters/${id}/workloads/${ref.kind}/${encodeURIComponent(ref.namespace)}/${encodeURIComponent(ref.name)}`;

// PVCRef identifies a PersistentVolumeClaim for the Storage page's detail endpoints.
export interface PVCRef {
  namespace: string;
  name: string;
}

const pvcPath = (id: string, ref: PVCRef) =>
  `/clusters/${id}/storage/pvcs/${encodeURIComponent(ref.namespace)}/${encodeURIComponent(ref.name)}`;

const scPath = (id: string, name: string) =>
  `/clusters/${id}/storage/storageclasses/${encodeURIComponent(name)}`;

// The Secrets page's paths. ConfigMaps and Secrets are both addressed by namespace/name.
const cmPath = (id: string, ns: string, name: string) =>
  `/clusters/${id}/configmaps/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`;

const secPath = (id: string, ns: string, name: string) =>
  `/clusters/${id}/secrets/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`;

// The Networking page's paths. Services and Gateways are addressed by namespace/name; a route needs
// its kind too, since the Gateway API route kinds are distinct resources sharing one page.
const svcPath = (id: string, ref: ObjectRef) =>
  `/clusters/${id}/networking/services/${encodeURIComponent(ref.namespace)}/${encodeURIComponent(ref.name)}`;

const gwPath = (id: string, ref: ObjectRef) =>
  `/clusters/${id}/networking/gateways/${encodeURIComponent(ref.namespace)}/${encodeURIComponent(ref.name)}`;

const routePath = (id: string, kind: RouteKind, ref: ObjectRef) =>
  `/clusters/${id}/networking/routes/${kind}/${encodeURIComponent(ref.namespace)}/${encodeURIComponent(ref.name)}`;

export const API_BASE = '/api';

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  // same-origin so the HttpOnly session cookie rides along (nginx in prod, Vite proxy in dev).
  const opts: RequestInit = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    (opts.headers as Record<string, string>)['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(`${API_BASE}${path}`, opts);
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = text;
  }
  if (!res.ok) {
    const msg =
      (data && typeof data === 'object' && 'error' in data && (data as { error: string }).error) ||
      res.statusText ||
      `request failed (${res.status})`;
    throw new ApiError(String(msg), res.status);
  }
  return data as T;
}

// fetchYaml GETs an endpoint that answers with text/yaml on success (the manifest endpoints) and a
// JSON `{"error": "..."}` body on failure, so the failure path still surfaces the API's own message.
async function fetchYaml(path: string): Promise<string> {
  const res = await fetch(`${API_BASE}${path}`, { credentials: 'same-origin' });
  const text = await res.text();
  if (!res.ok) {
    let msg = res.statusText || `could not load YAML (${res.status})`;
    try {
      const data = JSON.parse(text);
      if (data && typeof data === 'object' && 'error' in data) msg = String(data.error);
    } catch {
      // Not JSON - keep the status text.
    }
    throw new ApiError(msg, res.status);
  }
  return text;
}

export const api = {
  // Which release the API is running, for the sidebar footer. Public, and read from the API rather
  // than baked into this bundle at build time: the portal and the API are separate images, so a
  // baked-in version would name the web image and not the platform the user is actually driving.
  version: () => request<VersionInfo>('GET', '/version'),
  // auth / session
  me: () => request<User>('GET', '/auth/me'),
  // Public, and the only call the login page can make before it has a session: it says whether this
  // deployment authenticates against a directory, and therefore whether to offer registration.
  // (Server capabilities normally ride on /catalog, but that needs a session - which is exactly
  // what we don't have here.)
  authConfig: () => request<AuthConfig>('GET', '/auth/config'),
  login: (username: string, password: string) =>
    request<User>('POST', '/auth/login', { username, password }),
  register: (username: string, password: string) =>
    request<User>('POST', '/auth/register', { username, password }),
  logout: () => request<void>('POST', '/auth/logout'),
  // The account page: identity, the caller's own groups with names resolved, and their quota.
  getProfile: () => request<ProfileReport>('GET', '/auth/profile'),

  // admin: user management
  listUsers: () => request<UsersReport>('GET', '/users'),
  updateUser: (id: string, req: UpdateUserRequest) => request<User>('PATCH', `/users/${id}`, req),
  deleteUser: (id: string) => request<void>('DELETE', `/users/${id}`),

  // admin: group management
  listGroups: () => request<GroupView[] | null>('GET', '/groups').then((g) => g ?? []),
  createGroup: (name: string) => request<Group>('POST', '/groups', { name }),
  renameGroup: (id: string, name: string) => request<Group>('PATCH', `/groups/${id}`, { name }),
  deleteGroup: (id: string) => request<void>('DELETE', `/groups/${id}`),

  getCatalog: () => request<Catalog>('GET', '/catalog'),
  getCapacity: () => request<CapacityReport>('GET', '/capacity'),

  // ---- custom catalogs (per-user add-on catalogs) ----
  listCustomCatalogs: () =>
    request<CustomCatalogView[] | null>('GET', '/custom-catalogs').then((c) => c ?? []),
  getCustomCatalog: (id: string) => request<CustomCatalogView>('GET', `/custom-catalogs/${id}`),
  createCustomCatalog: (name: string) =>
    request<CustomCatalogView>('POST', '/custom-catalogs', { name }),
  renameCustomCatalog: (id: string, name: string) =>
    request<CustomCatalogView>('PATCH', `/custom-catalogs/${id}`, { name }),
  deleteCustomCatalog: (id: string) => request<void>('DELETE', `/custom-catalogs/${id}`),
  addCustomAddon: (id: string, addon: CustomAddon) =>
    request<CustomCatalogView>('POST', `/custom-catalogs/${id}/addons`, addon),
  updateCustomAddon: (id: string, name: string, addon: CustomAddon) =>
    request<CustomCatalogView>('PUT', `/custom-catalogs/${id}/addons/${encodeURIComponent(name)}`, addon),
  removeCustomAddon: (id: string, name: string) =>
    request<CustomCatalogView>('DELETE', `/custom-catalogs/${id}/addons/${encodeURIComponent(name)}`),
  // Fetch a chart's default values (also validates the repo/chart/version).
  fetchChartValues: (repo: string, chart: string, version: string) =>
    request<{ values: string }>('POST', '/custom-catalogs/chart-values', { repo, chart, version }).then(
      (r) => r.values,
    ),

  listClusters: () => request<Cluster[] | null>('GET', '/clusters').then((c) => c ?? []),
  getCluster: (id: string) => request<Cluster>('GET', `/clusters/${id}`),
  createCluster: (req: CreateClusterRequest) => request<Cluster>('POST', '/clusters', req),
  updateCluster: (id: string, req: UpdateClusterRequest) =>
    request<Cluster>('PATCH', `/clusters/${id}`, req),
  deleteCluster: (id: string) => request<void>('DELETE', `/clusters/${id}`),

  // ---- extra node disks (domain.NodeDisk) ----
  // Narrow add/remove rather than the whole-list PATCH node pools use: a disk belongs to ONE node,
  // and a lost update on a whole-list replace would silently destroy another disk - and its data.
  addNodeDisk: (id: string, req: AddNodeDiskRequest) =>
    request<Cluster>('POST', `/clusters/${id}/disks`, req),
  // Marks the disk for removal; the reconciler unmounts it in the guest before detaching it. THIS
  // DESTROYS THE DISK'S DATA.
  removeNodeDisk: (id: string, vmName: string, disk: string) =>
    request<Cluster>(
      'DELETE',
      `/clusters/${id}/nodes/${encodeURIComponent(vmName)}/disks/${encodeURIComponent(disk)}`,
    ),

  // ---- add-on values (the in-browser editor; addon-values seam) ----
  // Wizard: catalog-scoped defaults for an add-on (optionally version-pinned by a bundle).
  getCatalogAddonValues: (name: string, bundle?: string) =>
    request<AddonValuesView>(
      'GET',
      `/catalog/addons/${encodeURIComponent(name)}/values${bundle ? `?bundle=${encodeURIComponent(bundle)}` : ''}`,
    ),
  // Cluster-scoped: same, plus this cluster's saved override + the add-on's phase.
  getClusterAddonValues: (id: string, name: string) =>
    request<AddonValuesView>('GET', `/clusters/${id}/addons/${encodeURIComponent(name)}/values`),
  // Save (or, with an empty string, reset) a per-cluster override; drives a reconciler helm upgrade.
  putClusterAddonValues: (id: string, name: string, values: string) =>
    request<Cluster>('PUT', `/clusters/${id}/addons/${encodeURIComponent(name)}/values`, { values }),

  getUpgrades: (id: string) =>
    request<Bundle[] | null>('GET', `/clusters/${id}/upgrades`).then((b) => b ?? []),
  getOperations: (id: string) =>
    request<Operation[] | null>('GET', `/clusters/${id}/operations`).then((o) => o ?? []),
  // 204 (no snapshot yet / metrics-server disabled) decodes to null.
  getMetrics: (id: string) =>
    request<MetricsSnapshot | null>('GET', `/clusters/${id}/metrics`).then((m) => m ?? null),
  // 204 (no snapshot yet / health disabled) decodes to null.
  getHealth: (id: string) =>
    request<HealthSnapshot | null>('GET', `/clusters/${id}/health`).then((h) => h ?? null),
  upgradeCluster: (id: string, bundle: string) =>
    request<Cluster>('POST', `/clusters/${id}/upgrades`, { bundle }),

  // kubeconfig is text/yaml, fetched directly (not JSON).
  getKubeconfig: async (id: string): Promise<string> => {
    const res = await fetch(`${API_BASE}/clusters/${id}/kubeconfig`, { credentials: 'same-origin' });
    if (!res.ok) throw new ApiError('kubeconfig not ready yet', res.status);
    return res.text();
  },

  // ---- workloads (the Workloads page; kube query seam) ----
  listNamespaces: (id: string) =>
    request<string[] | null>('GET', `/clusters/${id}/namespaces`).then((n) => n ?? []),
  listWorkloads: (id: string, namespace?: string) =>
    request<WorkloadSummary[] | null>(
      'GET',
      `/clusters/${id}/workloads${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`,
    ).then((w) => w ?? []),
  getWorkload: (id: string, ref: WorkloadRef) => request<WorkloadDetail>('GET', wlPath(id, ref)),
  getWorkloadEvents: (id: string, ref: WorkloadRef) =>
    request<WorkloadEvent[] | null>('GET', `${wlPath(id, ref)}/events`).then((e) => e ?? []),
  scaleWorkload: (id: string, ref: WorkloadRef, replicas: number) =>
    request<void>('POST', `${wlPath(id, ref)}/scale`, { replicas }),

  // Workload YAML is text/yaml, fetched directly (not JSON).
  getWorkloadManifest: (id: string, ref: WorkloadRef) => fetchYaml(`${wlPath(id, ref)}/manifest`),

  // ---- storage (the Storage page; same kube query seam, storage half) ----
  listPVCs: (id: string, namespace?: string) =>
    request<PVCSummary[] | null>(
      'GET',
      `/clusters/${id}/storage/pvcs${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`,
    ).then((p) => p ?? []),
  getPVC: (id: string, ref: PVCRef) => request<PVCDetail>('GET', pvcPath(id, ref)),
  getPVCEvents: (id: string, ref: PVCRef) =>
    request<WorkloadEvent[] | null>('GET', `${pvcPath(id, ref)}/events`).then((e) => e ?? []),
  getPVCManifest: (id: string, ref: PVCRef) => fetchYaml(`${pvcPath(id, ref)}/manifest`),
  listStorageClasses: (id: string) =>
    request<StorageClass[] | null>('GET', `/clusters/${id}/storage/storageclasses`).then((s) => s ?? []),
  getStorageClassManifest: (id: string, name: string) => fetchYaml(`${scPath(id, name)}/manifest`),

  // ---- networking (the Networking page; same kube query seam, network half) ----
  getNetworkOverview: (id: string) =>
    request<NetworkOverview>('GET', `/clusters/${id}/networking/overview`),
  listServices: (id: string, namespace?: string) =>
    request<ServiceSummary[] | null>(
      'GET',
      `/clusters/${id}/networking/services${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`,
    ).then((s) => s ?? []),
  getService: (id: string, ref: ObjectRef) => request<ServiceDetail>('GET', svcPath(id, ref)),
  getServiceEvents: (id: string, ref: ObjectRef) =>
    request<WorkloadEvent[] | null>('GET', `${svcPath(id, ref)}/events`).then((e) => e ?? []),
  getServiceManifest: (id: string, ref: ObjectRef) => fetchYaml(`${svcPath(id, ref)}/manifest`),
  listGateways: (id: string) =>
    request<GatewaySummary[] | null>('GET', `/clusters/${id}/networking/gateways`).then((g) => g ?? []),
  getGatewayManifest: (id: string, ref: ObjectRef) => fetchYaml(`${gwPath(id, ref)}/manifest`),
  listRoutes: (id: string, namespace?: string) =>
    request<RouteSummary[] | null>(
      'GET',
      `/clusters/${id}/networking/routes${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`,
    ).then((r) => r ?? []),
  getRouteManifest: (id: string, kind: RouteKind, ref: ObjectRef) =>
    fetchYaml(`${routePath(id, kind, ref)}/manifest`),

  // ---- config & secrets (the Secrets page; same kube query seam, config half) ----
  listConfigMaps: (id: string, namespace?: string) =>
    request<ConfigMapSummary[] | null>(
      'GET',
      `/clusters/${id}/configmaps${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`,
    ).then((c) => c ?? []),
  getConfigMap: (id: string, ns: string, name: string) =>
    request<ConfigMapDetail>('GET', cmPath(id, ns, name)),
  getConfigMapManifest: (id: string, ns: string, name: string) => fetchYaml(`${cmPath(id, ns, name)}/manifest`),
  listSecrets: (id: string, namespace?: string) =>
    request<SecretSummary[] | null>(
      'GET',
      `/clusters/${id}/secrets${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`,
    ).then((s) => s ?? []),
  getSecret: (id: string, ns: string, name: string) => request<SecretDetail>('GET', secPath(id, ns, name)),
  getSecretManifest: (id: string, ns: string, name: string) => fetchYaml(`${secPath(id, ns, name)}/manifest`),
  // The "View in Vault" handoff: a short-lived, access-scoped Vault token + the UI URL for the path.
  getVaultSession: (id: string) => request<VaultSession>('GET', `/clusters/${id}/vault-session`),

  // Absolute ws(s):// URL for the per-pod log stream WebSocket (same-origin, like shellUrl).
  workloadLogsUrl: (
    id: string,
    ref: WorkloadRef,
    opts: { pod: string; container?: string; tail?: number; follow?: boolean },
  ) => {
    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
    const q = new URLSearchParams({ pod: opts.pod });
    if (opts.container) q.set('container', opts.container);
    if (opts.tail) q.set('tail', String(opts.tail));
    if (opts.follow) q.set('follow', '1');
    return `${proto}://${window.location.host}${API_BASE}${wlPath(id, ref)}/logs?${q.toString()}`;
  },

  // ---- monitoring (the Monitoring page; PromQL query seam) ----
  getMonitoringMeta: (id: string) => request<MonitoringMeta>('GET', `/clusters/${id}/monitoring`),
  // The Storage page's "Open UI" link (Longhorn) and whether this cluster can serve it.
  getStorageApps: (id: string) => request<StorageAppsMeta>('GET', `/clusters/${id}/storage/apps`),
  getMonitoringTab: (id: string, tab: string, window: string) =>
    request<MonitoringTabData>('GET', `/clusters/${id}/monitoring/${tab}?window=${encodeURIComponent(window)}`),

  // ---- security (the Security page; Trivy CRD query seam) ----
  getSecurityMeta: (id: string) => request<SecurityMeta>('GET', `/clusters/${id}/security`),
  getSecurityOverview: (id: string) =>
    request<SecurityOverview>('GET', `/clusters/${id}/security/overview`),
  listSecurityReports: (id: string, kind: SecurityKind) =>
    request<SecurityReport[] | null>('GET', `/clusters/${id}/security/reports/${kind}`).then((r) => r ?? []),
  getSecurityReport: (id: string, kind: SecurityKind, namespace: string, name: string) => {
    const q = new URLSearchParams({ name });
    if (namespace) q.set('namespace', namespace);
    return request<SecurityReportDetail>('GET', `/clusters/${id}/security/report/${kind}?${q.toString()}`);
  },

  // ---- audit (the Audit tab; API-server audit query seam) ----
  getAudit: (id: string, params: AuditQuery = {}) => {
    const q = new URLSearchParams();
    if (params.limit) q.set('limit', String(params.limit));
    if (params.verb) q.set('verb', params.verb);
    if (params.namespace) q.set('namespace', params.namespace);
    if (params.user) q.set('user', params.user);
    if (params.resource) q.set('resource', params.resource);
    if (params.q) q.set('q', params.q);
    const qs = q.toString();
    return request<AuditPage>('GET', `/clusters/${id}/audit${qs ? `?${qs}` : ''}`);
  },

  // Absolute URL for the SSE stream (used by EventSource, which can't take relative helpers).
  eventsUrl: (id: string) => `${API_BASE}/clusters/${id}/events`,

  // Absolute ws(s):// URL for the interactive cluster shell WebSocket. Same-origin as the SPA, so
  // it works behind nginx (prod) and the Vite dev proxy (ws: true) alike.
  shellUrl: (id: string) => {
    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
    return `${proto}://${window.location.host}${API_BASE}/clusters/${id}/shell`;
  },

  // Absolute ws(s):// URL for a node's SSH WebSocket (the Nodes tab SSH button). Same-origin, like
  // shellUrl. vmName is the node's stable identity; the API resolves its IP server-side.
  nodeSshUrl: (id: string, vmName: string) => {
    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
    return `${proto}://${window.location.host}${API_BASE}/clusters/${id}/nodes/${encodeURIComponent(vmName)}/ssh`;
  },
};
