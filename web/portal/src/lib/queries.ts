// TanStack Query hooks over the API. Lists/details poll every 2s (matching the old UI's cadence)
// so the reconciler's progress shows up live; catalog is effectively static. Mutations invalidate
// the relevant caches and raise a notification so the UI reacts without manual refetch wiring.

import {
  useMutation,
  useQuery,
  useQueryClient,
  keepPreviousData,
} from '@tanstack/react-query';
import { notifications } from '@mantine/notifications';
import { api, ApiError } from './api';
import type { WorkloadRef, PVCRef } from './api';
import type {
  CreateClusterRequest,
  UpdateClusterRequest,
  UpdateUserRequest,
  AddNodeDiskRequest,
  SecurityKind,
  AuditQuery,
  CustomAddon,
  ObjectRef,
  RouteKind,
} from './types';

const POLL_MS = 2000;

export const qk = {
  version: ['version'] as const,
  clusters: ['clusters'] as const,
  cluster: (id: string) => ['clusters', id] as const,
  capacity: ['capacity'] as const,
  profile: ['profile'] as const,
  catalog: ['catalog'] as const,
  customCatalogs: ['custom-catalogs'] as const,
  users: ['users'] as const,
  groups: ['groups'] as const,
  catalogAddonValues: (name: string, bundle: string) => ['catalog', 'addon-values', name, bundle] as const,
  clusterAddonValues: (id: string, name: string) => ['clusters', id, 'addon-values', name] as const,
  upgrades: (id: string) => ['clusters', id, 'upgrades'] as const,
  operations: (id: string) => ['clusters', id, 'operations'] as const,
  metrics: (id: string) => ['clusters', id, 'metrics'] as const,
  health: (id: string) => ['clusters', id, 'health'] as const,
  namespaces: (id: string) => ['clusters', id, 'namespaces'] as const,
  workloads: (id: string, ns: string) => ['clusters', id, 'workloads', ns] as const,
  workload: (id: string, ref: WorkloadRef) =>
    ['clusters', id, 'workloads', ref.kind, ref.namespace, ref.name] as const,
  workloadEvents: (id: string, ref: WorkloadRef) =>
    ['clusters', id, 'workloads', ref.kind, ref.namespace, ref.name, 'events'] as const,
  pvcs: (id: string, ns: string) => ['clusters', id, 'storage', 'pvcs', ns] as const,
  pvc: (id: string, ref: PVCRef) => ['clusters', id, 'storage', 'pvc', ref.namespace, ref.name] as const,
  pvcEvents: (id: string, ref: PVCRef) =>
    ['clusters', id, 'storage', 'pvc', ref.namespace, ref.name, 'events'] as const,
  pvcManifest: (id: string, ref: PVCRef) =>
    ['clusters', id, 'storage', 'pvc', ref.namespace, ref.name, 'manifest'] as const,
  storageClasses: (id: string) => ['clusters', id, 'storage', 'storageclasses'] as const,
  storageClassManifest: (id: string, name: string) =>
    ['clusters', id, 'storage', 'storageclasses', name, 'manifest'] as const,
  networkOverview: (id: string) => ['clusters', id, 'networking', 'overview'] as const,
  services: (id: string, ns: string) => ['clusters', id, 'networking', 'services', ns] as const,
  service: (id: string, ref: ObjectRef) =>
    ['clusters', id, 'networking', 'service', ref.namespace, ref.name] as const,
  serviceEvents: (id: string, ref: ObjectRef) =>
    ['clusters', id, 'networking', 'service', ref.namespace, ref.name, 'events'] as const,
  serviceManifest: (id: string, ref: ObjectRef) =>
    ['clusters', id, 'networking', 'service', ref.namespace, ref.name, 'manifest'] as const,
  gateways: (id: string) => ['clusters', id, 'networking', 'gateways'] as const,
  gatewayManifest: (id: string, ref: ObjectRef) =>
    ['clusters', id, 'networking', 'gateway', ref.namespace, ref.name, 'manifest'] as const,
  routes: (id: string, ns: string) => ['clusters', id, 'networking', 'routes', ns] as const,
  routeManifest: (id: string, kind: string, ref: ObjectRef) =>
    ['clusters', id, 'networking', 'route', kind, ref.namespace, ref.name, 'manifest'] as const,
  configMaps: (id: string, ns: string) => ['clusters', id, 'configmaps', ns] as const,
  configMap: (id: string, ns: string, name: string) => ['clusters', id, 'configmap', ns, name] as const,
  configMapManifest: (id: string, ns: string, name: string) =>
    ['clusters', id, 'configmap', ns, name, 'manifest'] as const,
  secrets: (id: string, ns: string) => ['clusters', id, 'secrets', ns] as const,
  secret: (id: string, ns: string, name: string) => ['clusters', id, 'secret', ns, name] as const,
  secretManifest: (id: string, ns: string, name: string) =>
    ['clusters', id, 'secret', ns, name, 'manifest'] as const,
  monitoringMeta: (id: string) => ['clusters', id, 'monitoring', 'meta'] as const,
  storageApps: (id: string) => ['clusters', id, 'storage', 'apps'] as const,
  monitoringTab: (id: string, tab: string, window: string) =>
    ['clusters', id, 'monitoring', tab, window] as const,
  securityMeta: (id: string) => ['clusters', id, 'security', 'meta'] as const,
  securityOverview: (id: string) => ['clusters', id, 'security', 'overview'] as const,
  securityReports: (id: string, kind: string) => ['clusters', id, 'security', 'reports', kind] as const,
  securityReport: (id: string, kind: string, namespace: string, name: string) =>
    ['clusters', id, 'security', 'report', kind, namespace, name] as const,
  audit: (id: string, params: AuditQuery) => ['clusters', id, 'audit', params] as const,
};

// METRICS_POLL_MS is slower than the reconcile-driven POLL_MS: usage is dashboard telemetry the
// backend itself only samples every ~15s, so hammering it faster gains nothing.
const METRICS_POLL_MS = 5000;

export function useClusters() {
  return useQuery({
    queryKey: qk.clusters,
    queryFn: api.listClusters,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useCluster(id: string | undefined) {
  return useQuery({
    queryKey: qk.cluster(id ?? ''),
    queryFn: () => api.getCluster(id as string),
    enabled: !!id,
    refetchInterval: POLL_MS,
  });
}

export function useCapacity() {
  return useQuery({
    queryKey: qk.capacity,
    queryFn: api.getCapacity,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// useProfile backs the account page. It polls like the other views so a quota grant or a group
// change made by an admin shows up without a reload.
export function useProfile() {
  return useQuery({
    queryKey: qk.profile,
    queryFn: api.getProfile,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useCatalog() {
  return useQuery({
    queryKey: qk.catalog,
    queryFn: api.getCatalog,
    staleTime: 5 * 60 * 1000,
  });
}

// The API's build identity, for the sidebar footer. Never refetched: a running process cannot
// change its own version, and an upgrade replaces the page along with the pod. A failure is not
// worth surfacing - the footer just stays empty.
export function useVersion() {
  return useQuery({
    queryKey: qk.version,
    queryFn: api.version,
    staleTime: Infinity,
    retry: false,
  });
}

// ---- custom catalogs (per-user add-on catalogs) ----

// All catalogs visible to the user (own + group-shared). Polls so a group-mate's edits show up live.
export function useCustomCatalogs() {
  return useQuery({
    queryKey: qk.customCatalogs,
    queryFn: api.listCustomCatalogs,
    refetchInterval: POLL_MS,
  });
}

// The mutations all return the updated catalog and invalidate the list, so every open view refreshes.
export function useCustomCatalogMutations() {
  const qc = useQueryClient();
  const invalidate = () => qc.invalidateQueries({ queryKey: qk.customCatalogs });
  const onError = (title: string) => (err: unknown) =>
    notifications.show({ color: 'red', title, message: errText(err) });

  const create = useMutation({
    mutationFn: (name: string) => api.createCustomCatalog(name),
    onSuccess: () => {
      notifications.show({ color: 'teal', title: 'Catalog created', message: 'Add Helm-chart add-ons to it.' });
      invalidate();
    },
    onError: onError('Could not create catalog'),
  });
  const rename = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => api.renameCustomCatalog(id, name),
    onSuccess: invalidate,
    onError: onError('Could not rename catalog'),
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.deleteCustomCatalog(id),
    onSuccess: () => {
      notifications.show({ color: 'orange', title: 'Catalog deleted', message: 'Existing clusters keep their installed add-ons.' });
      invalidate();
    },
    onError: onError('Could not delete catalog'),
  });
  const addAddon = useMutation({
    mutationFn: ({ id, addon }: { id: string; addon: CustomAddon }) => api.addCustomAddon(id, addon),
    onSuccess: invalidate,
    onError: onError('Could not add add-on'),
  });
  const updateAddon = useMutation({
    mutationFn: ({ id, name, addon }: { id: string; name: string; addon: CustomAddon }) =>
      api.updateCustomAddon(id, name, addon),
    onSuccess: invalidate,
    onError: onError('Could not update add-on'),
  });
  const removeAddon = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => api.removeCustomAddon(id, name),
    onSuccess: invalidate,
    onError: onError('Could not remove add-on'),
  });
  return { create, rename, remove, addAddon, updateAddon, removeAddon };
}

// ---- add-on values editor ----

// Catalog-scoped default values for an add-on (wizard). Version-pinned by the chosen bundle. Only
// fetched while the editor modal is open (enabled), and cached - the chart defaults don't change.
export function useCatalogAddonValues(name: string, bundle: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.catalogAddonValues(name, bundle),
    queryFn: () => api.getCatalogAddonValues(name, bundle || undefined),
    enabled: !!name && enabled,
    staleTime: 5 * 60 * 1000,
  });
}

// Cluster-scoped values for an installed add-on (Add-ons tab): defaults + this cluster's saved
// override + the add-on's phase. Fetched while the editor modal is open.
export function useClusterAddonValues(id: string | undefined, name: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.clusterAddonValues(id ?? '', name),
    queryFn: () => api.getClusterAddonValues(id as string, name),
    enabled: !!id && !!name && enabled,
    staleTime: 10 * 1000,
  });
}

// Saves (or resets) a per-cluster add-on values override; the server bumps the generation and the
// reconciler runs a helm upgrade, so we invalidate the cluster + its operations to watch it converge.
export function useSetClusterAddonValues(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, values }: { name: string; values: string }) =>
      api.putClusterAddonValues(id, name, values),
    onSuccess: (_c, { name, values }) => {
      notifications.show({
        color: 'blue',
        title: values.trim() ? 'Add-on values updated' : 'Add-on values reset',
        message: `The reconciler is applying the new values for ${name}.`,
      });
      qc.invalidateQueries({ queryKey: qk.cluster(id) });
      qc.invalidateQueries({ queryKey: qk.clusterAddonValues(id, name) });
      qc.invalidateQueries({ queryKey: qk.operations(id) });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not update add-on values', message: errText(err) }),
  });
}

export function useUpgrades(id: string | undefined) {
  return useQuery({
    queryKey: qk.upgrades(id ?? ''),
    queryFn: () => api.getUpgrades(id as string),
    enabled: !!id,
    staleTime: 60 * 1000,
  });
}

// Polls the cluster's action history so in-progress operations (and their completion) show live.
export function useOperations(id: string | undefined) {
  return useQuery({
    queryKey: qk.operations(id ?? ''),
    queryFn: () => api.getOperations(id as string),
    enabled: !!id,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// Polls a cluster's live resource-usage snapshot. Only enabled when it makes sense to ask
// (caller passes enabled=Ready && metrics-server installed), so we don't poll for 204s forever.
export function useMetrics(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: qk.metrics(id ?? ''),
    queryFn: () => api.getMetrics(id as string),
    enabled: !!id && enabled,
    refetchInterval: METRICS_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// Polls a cluster's live health snapshot. Gated on Ready (caller passes enabled), matching the
// backend's health ticker - only Ready clusters are evaluated, so before that we'd just get 204s.
export function useHealth(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: qk.health(id ?? ''),
    queryFn: () => api.getHealth(id as string),
    enabled: !!id && enabled,
    refetchInterval: METRICS_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// ---- workloads (the Workloads page) ----

// Poll cadence for the (kubectl-backed) workloads views: live enough to watch a scale/rollout,
// slow enough not to hammer the worker exec agent with kubectl invocations.
const WORKLOADS_POLL_MS = 4000;

export function useNamespaces(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: qk.namespaces(id ?? ''),
    queryFn: () => api.listNamespaces(id as string),
    enabled: !!id && enabled,
    staleTime: 30 * 1000,
    placeholderData: keepPreviousData,
  });
}

export function useWorkloads(id: string | undefined, namespace: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.workloads(id ?? '', namespace),
    queryFn: () => api.listWorkloads(id as string, namespace || undefined),
    enabled: !!id && enabled,
    refetchInterval: WORKLOADS_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useWorkload(id: string | undefined, ref: WorkloadRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.workload(id ?? '', ref),
    queryFn: () => api.getWorkload(id as string, ref),
    enabled: !!id && enabled,
    refetchInterval: WORKLOADS_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useWorkloadEvents(id: string | undefined, ref: WorkloadRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.workloadEvents(id ?? '', ref),
    queryFn: () => api.getWorkloadEvents(id as string, ref),
    enabled: !!id && enabled,
    refetchInterval: WORKLOADS_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// ---- storage (the Storage page) ----

// Storage objects change far more slowly than workloads - a claim binds once and then sits there -
// so the lists poll at a relaxed cadence, and a claim's YAML (immutable in practice) is not polled
// at all. Same worker exec agent underneath, so there's no reason to ask more often than the data
// can change.
const STORAGE_POLL_MS = 10000;

export function usePVCs(id: string | undefined, namespace: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.pvcs(id ?? '', namespace),
    queryFn: () => api.listPVCs(id as string, namespace || undefined),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function usePVC(id: string | undefined, ref: PVCRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.pvc(id ?? '', ref),
    queryFn: () => api.getPVC(id as string, ref),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function usePVCEvents(id: string | undefined, ref: PVCRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.pvcEvents(id ?? '', ref),
    queryFn: () => api.getPVCEvents(id as string, ref),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function usePVCManifest(id: string | undefined, ref: PVCRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.pvcManifest(id ?? '', ref),
    queryFn: () => api.getPVCManifest(id as string, ref),
    enabled: !!id && enabled,
    staleTime: 30 * 1000,
  });
}

// The Storage page's Longhorn UI link. Polled on the reconcile cadence rather than the storage one:
// what changes here is whether the add-on has finished installing, not the storage itself.
export function useStorageApps(id: string | undefined) {
  return useQuery({
    queryKey: qk.storageApps(id ?? ''),
    queryFn: () => api.getStorageApps(id as string),
    enabled: !!id,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useStorageClasses(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: qk.storageClasses(id ?? ''),
    queryFn: () => api.listStorageClasses(id as string),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useStorageClassManifest(id: string | undefined, name: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.storageClassManifest(id ?? '', name),
    queryFn: () => api.getStorageClassManifest(id as string, name),
    enabled: !!id && !!name && enabled,
    staleTime: 30 * 1000,
  });
}

// ---- config & secrets (the Secrets page) ----
//
// Same relaxed cadence as storage: a ConfigMap/Secret changes when someone (or ESO) writes it, not on
// its own. Secret values are never fetched - the detail carries only key names and byte lengths.

export function useConfigMaps(id: string | undefined, namespace: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.configMaps(id ?? '', namespace),
    queryFn: () => api.listConfigMaps(id as string, namespace || undefined),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useConfigMap(id: string | undefined, ns: string, name: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.configMap(id ?? '', ns, name),
    queryFn: () => api.getConfigMap(id as string, ns, name),
    enabled: !!id && !!name && enabled,
    staleTime: 15 * 1000,
  });
}

export function useConfigMapManifest(id: string | undefined, ns: string, name: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.configMapManifest(id ?? '', ns, name),
    queryFn: () => api.getConfigMapManifest(id as string, ns, name),
    enabled: !!id && !!name && enabled,
    staleTime: 15 * 1000,
  });
}

export function useSecrets(id: string | undefined, namespace: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.secrets(id ?? '', namespace),
    queryFn: () => api.listSecrets(id as string, namespace || undefined),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useSecret(id: string | undefined, ns: string, name: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.secret(id ?? '', ns, name),
    queryFn: () => api.getSecret(id as string, ns, name),
    enabled: !!id && !!name && enabled,
    staleTime: 15 * 1000,
  });
}

export function useSecretManifest(id: string | undefined, ns: string, name: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.secretManifest(id ?? '', ns, name),
    queryFn: () => api.getSecretManifest(id as string, ns, name),
    enabled: !!id && !!name && enabled,
    staleTime: 15 * 1000,
  });
}

// ---- networking (the Networking page) ----
//
// Same cadence as storage: routes and Services change when a user applies something, not on their
// own, so the same worker exec agent is polled at the same unhurried rate.

export function useNetworkOverview(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: qk.networkOverview(id ?? ''),
    queryFn: () => api.getNetworkOverview(id as string),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useServices(id: string | undefined, namespace: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.services(id ?? '', namespace),
    queryFn: () => api.listServices(id as string, namespace || undefined),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useService(id: string | undefined, ref: ObjectRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.service(id ?? '', ref),
    queryFn: () => api.getService(id as string, ref),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useServiceEvents(id: string | undefined, ref: ObjectRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.serviceEvents(id ?? '', ref),
    queryFn: () => api.getServiceEvents(id as string, ref),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useServiceManifest(id: string | undefined, ref: ObjectRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.serviceManifest(id ?? '', ref),
    queryFn: () => api.getServiceManifest(id as string, ref),
    enabled: !!id && enabled,
    staleTime: 30 * 1000,
  });
}

export function useGateways(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: qk.gateways(id ?? ''),
    queryFn: () => api.listGateways(id as string),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useGatewayManifest(id: string | undefined, ref: ObjectRef, enabled: boolean) {
  return useQuery({
    queryKey: qk.gatewayManifest(id ?? '', ref),
    queryFn: () => api.getGatewayManifest(id as string, ref),
    enabled: !!id && enabled,
    staleTime: 30 * 1000,
  });
}

export function useRoutes(id: string | undefined, namespace: string, enabled: boolean) {
  return useQuery({
    queryKey: qk.routes(id ?? '', namespace),
    queryFn: () => api.listRoutes(id as string, namespace || undefined),
    enabled: !!id && enabled,
    refetchInterval: STORAGE_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useRouteManifest(
  id: string | undefined,
  kind: RouteKind,
  ref: ObjectRef,
  enabled: boolean,
) {
  return useQuery({
    queryKey: qk.routeManifest(id ?? '', kind, ref),
    queryFn: () => api.getRouteManifest(id as string, kind, ref),
    enabled: !!id && enabled,
    staleTime: 30 * 1000,
  });
}

// ---- monitoring (the Monitoring page) ----

// MONITORING_POLL_MS is a slower cadence than the workloads views: the panels are dashboard
// telemetry, and each tab refresh fans out several kubectl-proxied PromQL queries at the worker, so
// there's nothing to gain from polling faster than the graphs meaningfully move.
const MONITORING_POLL_MS = 12000;

// Fetches the tab list + whether the cluster is Ready and has the monitoring stack installed, so the
// page can render its gating states before issuing any query. Static-ish, but cheap to re-check.
export function useMonitoringMeta(id: string | undefined) {
  return useQuery({
    queryKey: qk.monitoringMeta(id ?? ''),
    queryFn: () => api.getMonitoringMeta(id as string),
    enabled: !!id,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// Polls one Monitoring tab's resolved panels for the selected time-range window. Gated (caller
// passes enabled = Ready && stack installed) so we don't poll for 409s. The window is part of the
// query key, so switching it refetches while keeping the previous panels on screen.
export function useMonitoringTab(
  id: string | undefined,
  tab: string,
  window: string,
  enabled: boolean,
) {
  return useQuery({
    queryKey: qk.monitoringTab(id ?? '', tab, window),
    queryFn: () => api.getMonitoringTab(id as string, tab, window),
    enabled: !!id && !!tab && !!window && enabled,
    refetchInterval: MONITORING_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// ---- security (the Security page) ----

// SECURITY_POLL_MS is a slow cadence: Trivy reports change only when the operator re-scans (minutes
// apart), and each refresh fans out several kubectl-proxied CRD reads at the worker.
const SECURITY_POLL_MS = 15000;

// Fetches the report-kind list + whether the cluster is Ready and has the Trivy Operator installed,
// so the page can render its gating states before issuing any query.
export function useSecurityMeta(id: string | undefined) {
  return useQuery({
    queryKey: qk.securityMeta(id ?? ''),
    queryFn: () => api.getSecurityMeta(id as string),
    enabled: !!id,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useSecurityOverview(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: qk.securityOverview(id ?? ''),
    queryFn: () => api.getSecurityOverview(id as string),
    enabled: !!id && enabled,
    refetchInterval: SECURITY_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useSecurityReports(id: string | undefined, kind: SecurityKind, enabled: boolean) {
  return useQuery({
    queryKey: qk.securityReports(id ?? '', kind),
    queryFn: () => api.listSecurityReports(id as string, kind),
    enabled: !!id && enabled,
    refetchInterval: SECURITY_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// Fetches one report's full finding list; enabled only while its detail drawer is open.
export function useSecurityReport(
  id: string | undefined,
  kind: SecurityKind,
  namespace: string,
  name: string,
  enabled: boolean,
) {
  return useQuery({
    queryKey: qk.securityReport(id ?? '', kind, namespace, name),
    queryFn: () => api.getSecurityReport(id as string, kind, namespace, name),
    enabled: !!id && !!name && enabled,
    staleTime: 10 * 1000,
  });
}

// ---- audit (the Audit tab) ----

// AUDIT_POLL_MS is a brisk cadence so the feed reads like a live tail - each refresh is a bounded
// `kubectl logs --tail` at the worker, and the fake advances one event roughly every 12s.
const AUDIT_POLL_MS = 5000;

// Polls the cluster's API-server audit events for the current filter set. Gated on enabled (the
// caller passes Ready) so we don't poll for 409s during bring-up. Filters are part of the query key,
// so changing one refetches while keepPreviousData holds the old rows on screen (no flicker).
export function useAudit(id: string | undefined, params: AuditQuery, enabled: boolean) {
  return useQuery({
    queryKey: qk.audit(id ?? '', params),
    queryFn: () => api.getAudit(id as string, params),
    enabled: !!id && enabled,
    refetchInterval: AUDIT_POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useScaleWorkload(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ ref, replicas }: { ref: WorkloadRef; replicas: number }) =>
      api.scaleWorkload(id, ref, replicas),
    onSuccess: (_res, { ref, replicas }) => {
      notifications.show({
        color: 'blue',
        title: 'Scaling workload',
        message: `${ref.name} → ${replicas} replica${replicas === 1 ? '' : 's'}.`,
      });
      qc.invalidateQueries({ queryKey: qk.workload(id, ref) });
      qc.invalidateQueries({ queryKey: ['clusters', id, 'workloads'] });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not scale workload', message: errText(err) }),
  });
}

function errText(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error) return err.message;
  return String(err);
}

export function useCreateCluster() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: CreateClusterRequest) => api.createCluster(req),
    onSuccess: (c) => {
      notifications.show({ color: 'teal', title: 'Cluster created', message: `${c.name} is being provisioned.` });
      qc.invalidateQueries({ queryKey: qk.clusters });
      qc.invalidateQueries({ queryKey: qk.capacity });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not create cluster', message: errText(err) }),
  });
}

// Extra node disks. Both mutations refresh the same keys as a cluster edit: they bump the
// generation (so the cluster goes Updating) and they spend disk quota (so /capacity moves).
export function useAddNodeDisk(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: AddNodeDiskRequest) => api.addNodeDisk(id, req),
    onSuccess: (_c, req) => {
      notifications.show({
        color: 'blue',
        title: 'Disk requested',
        message: `${req.name} (${req.size_gb} GB) will be attached to ${req.vm_name} and mounted at ${req.mount_path}.`,
      });
      qc.invalidateQueries({ queryKey: qk.cluster(id) });
      qc.invalidateQueries({ queryKey: qk.clusters });
      qc.invalidateQueries({ queryKey: qk.capacity });
      qc.invalidateQueries({ queryKey: qk.operations(id) });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Disk rejected', message: errText(err) }),
  });
}

export function useRemoveNodeDisk(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ vmName, disk }: { vmName: string; disk: string }) =>
      api.removeNodeDisk(id, vmName, disk),
    onSuccess: (_c, { disk }) => {
      notifications.show({
        color: 'blue',
        title: 'Disk removal requested',
        message: `${disk} is being unmounted and destroyed.`,
      });
      qc.invalidateQueries({ queryKey: qk.cluster(id) });
      qc.invalidateQueries({ queryKey: qk.clusters });
      qc.invalidateQueries({ queryKey: qk.capacity });
      qc.invalidateQueries({ queryKey: qk.operations(id) });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Removal rejected', message: errText(err) }),
  });
}

export function useUpdateCluster(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: UpdateClusterRequest) => api.updateCluster(id, req),
    onSuccess: () => {
      notifications.show({ color: 'blue', title: 'Changes applied', message: 'The reconciler is converging the cluster.' });
      qc.invalidateQueries({ queryKey: qk.cluster(id) });
      qc.invalidateQueries({ queryKey: qk.clusters });
      qc.invalidateQueries({ queryKey: qk.capacity });
      qc.invalidateQueries({ queryKey: qk.operations(id) });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Update rejected', message: errText(err) }),
  });
}

export function useUpgradeCluster(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (bundle: string) => api.upgradeCluster(id, bundle),
    onSuccess: (c) => {
      notifications.show({
        color: 'cyan',
        title: 'Upgrade started',
        message: `Promoting ${c.name} toward ${c.target_bundle ?? 'the target bundle'}. Watch the Activity tab.`,
      });
      qc.invalidateQueries({ queryKey: qk.cluster(id) });
      qc.invalidateQueries({ queryKey: qk.clusters });
      qc.invalidateQueries({ queryKey: qk.upgrades(id) });
      qc.invalidateQueries({ queryKey: qk.operations(id) });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Upgrade rejected', message: errText(err) }),
  });
}

export function useDeleteCluster() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteCluster(id),
    onSuccess: () => {
      notifications.show({ color: 'orange', title: 'Deleting cluster', message: 'Tearing down the cluster and its VMs.' });
      qc.invalidateQueries({ queryKey: qk.clusters });
      qc.invalidateQueries({ queryKey: qk.capacity });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not delete cluster', message: errText(err) }),
  });
}

// ---- admin: user management (GET/PATCH/DELETE /users) ----

export function useUsers(enabled: boolean) {
  return useQuery({
    queryKey: qk.users,
    queryFn: api.listUsers,
    enabled,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

// useUpdateUser covers both quota grants and group (re)assignment - the PATCH /users/{id} request
// accepts either or both fields at once (see app.UpdateUserRequest).
export function useUpdateUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, req }: { id: string; req: UpdateUserRequest }) => api.updateUser(id, req),
    onSuccess: (u) => {
      notifications.show({ color: 'teal', title: 'Account updated', message: `Updated ${u.username}.` });
      qc.invalidateQueries({ queryKey: qk.users });
      qc.invalidateQueries({ queryKey: qk.capacity });
      qc.invalidateQueries({ queryKey: qk.clusters });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not update account', message: errText(err) }),
  });
}

export function useDeleteUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteUser(id),
    onSuccess: () => {
      notifications.show({ color: 'orange', title: 'Account deleted', message: 'Its clusters are being torn down.' });
      qc.invalidateQueries({ queryKey: qk.users });
      qc.invalidateQueries({ queryKey: qk.clusters });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not delete account', message: errText(err) }),
  });
}

// ---- admin: group management (GET/POST/PATCH/DELETE /groups) ----

export function useGroups(enabled: boolean) {
  return useQuery({
    queryKey: qk.groups,
    queryFn: api.listGroups,
    enabled,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });
}

export function useCreateGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.createGroup(name),
    onSuccess: (g) => {
      notifications.show({ color: 'teal', title: 'Group created', message: g.name });
      qc.invalidateQueries({ queryKey: qk.groups });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not create group', message: errText(err) }),
  });
}

export function useRenameGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => api.renameGroup(id, name),
    onSuccess: (g) => {
      notifications.show({ color: 'teal', title: 'Group renamed', message: g.name });
      qc.invalidateQueries({ queryKey: qk.groups });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not rename group', message: errText(err) }),
  });
}

export function useDeleteGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteGroup(id),
    onSuccess: () => {
      notifications.show({ color: 'orange', title: 'Group deleted', message: 'Its members were removed from the group.' });
      qc.invalidateQueries({ queryKey: qk.groups });
      qc.invalidateQueries({ queryKey: qk.users });
      qc.invalidateQueries({ queryKey: qk.clusters });
    },
    onError: (err) =>
      notifications.show({ color: 'red', title: 'Could not delete group', message: errText(err) }),
  });
}
