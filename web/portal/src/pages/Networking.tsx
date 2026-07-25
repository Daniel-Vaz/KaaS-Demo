// The Networking page: a cluster's north-south networking, from the platform's default ingress
// contract down to the raw objects.
//
// Four tabs. Overview is the point of the page - the reserved gateway address, the wildcard DNS
// record, and every application published through them (see components/network/Overview.tsx). The
// other three are the objects behind it: Services (core Kubernetes), and the Gateway API's Gateways
// and Routes, which come from the envoy-gateway add-on and are simply empty without it.
//
// Same shape as Storage/Workloads: cluster + namespace pickers, a URL that carries the whole
// selection so the page is deep-linkable, and a right-hand drawer per object kind.

import { useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router';
import {
  Group,
  Title,
  Text,
  Select,
  Tabs,
  Table,
  Badge,
  Card,
  TextInput,
  Tooltip,
  Skeleton,
  Anchor,
  Alert,
} from '@mantine/core';
import {
  IconSearch,
  IconAffiliate,
  IconAlertTriangle,
  IconServer2,
  IconRoute,
  IconWorld,
} from '@tabler/icons-react';
import {
  useClusters,
  useNamespaces,
  useNetworkOverview,
  useServices,
  useGateways,
  useRoutes,
} from '../lib/queries';
import { ApiError } from '../lib/api';
import { EmptyState } from '../components/EmptyState';
import { activeClusters, clusterUsable } from '../lib/cluster';
import { relative } from '../lib/format';
import {
  serviceTypeColor,
  serviceTypeLabel,
  formatPorts,
  readyColor,
  routeKindLabel,
  backendsText,
} from '../lib/network';
import { NetworkingOverview } from '../components/network/Overview';
import { ServiceDrawer } from '../components/network/ServiceDrawer';
import { GatewayDrawer } from '../components/network/GatewayDrawer';
import { RouteDrawer } from '../components/network/RouteDrawer';
import type { ServiceSummary, GatewaySummary, RouteSummary } from '../lib/types';

// All-namespaces sentinel for the namespace <Select> (empty string maps to "all" on the API).
const ALL_NS = '';

export function Networking() {
  const [params, setParams] = useSearchParams();
  const { data: clustersRaw, isLoading: clustersLoading } = useClusters();
  const clusters = useMemo(() => activeClusters(clustersRaw ?? []), [clustersRaw]);

  // Selected cluster: from the URL, else the first usable cluster, else the first - mirroring the
  // Workloads/Storage/Security pages' resolution.
  const urlCluster = params.get('cluster') ?? '';
  const selected = clusters.find((c) => c.id === urlCluster);
  const fallback = clusters.find(clusterUsable) ?? clusters[0];
  const cluster = selected ?? fallback;
  const clusterId = cluster?.id;
  const ready = !!cluster && clusterUsable(cluster);

  useEffect(() => {
    if (cluster && cluster.id !== urlCluster) {
      setParams(
        (p) => {
          const next = new URLSearchParams(p);
          next.set('cluster', cluster.id);
          return next;
        },
        { replace: true },
      );
    }
  }, [cluster, urlCluster, setParams]);

  const namespace = params.get('namespace') ?? ALL_NS;
  const tab = params.get('tab') || 'overview';
  const q = params.get('q') ?? '';

  const setParam = (key: string, value: string) =>
    setParams((p) => {
      const next = new URLSearchParams(p);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });

  const { data: namespaces } = useNamespaces(clusterId, ready);

  // The Overview's exposed-apps table links a hostname to the route that publishes it; clicking one
  // jumps to the Routes tab with that route's drawer open, so "what serves this URL" is one click.
  const [openRoute, setOpenRoute] = useState<RouteSummary | null>(null);
  const [pendingRoute, setPendingRoute] = useState<{ namespace: string; name: string } | null>(null);

  // The namespace picker bears on Services and Routes only - Gateways are listed cluster-wide (there
  // are few of them, and the one that matters lives in the add-on's namespace, not the user's).
  const namespaceScoped = tab === 'services' || tab === 'routes';

  return (
    <>
      <Group justify="space-between" mb="lg" align="flex-start">
        <div>
          <Title order={2}>Networking</Title>
          <Text c="dimmed" size="sm">
            How traffic reaches a cluster: its default gateway and DNS, the applications published
            through them, and the services, gateways and routes underneath.
          </Text>
        </div>
      </Group>

      <Group mb="md" gap="sm" align="flex-end" wrap="wrap">
        <Select
          label="Cluster"
          placeholder={clustersLoading ? 'Loading…' : 'Select a cluster'}
          data={clusters.map((c) => ({
            value: c.id,
            label: clusterUsable(c) ? c.name : `${c.name} (${c.phase})`,
          }))}
          value={clusterId ?? null}
          onChange={(v) => {
            if (v) {
              setParams((p) => {
                const next = new URLSearchParams(p);
                next.set('cluster', v);
                next.delete('namespace');
                return next;
              });
            }
          }}
          searchable
          w={260}
          nothingFoundMessage="No clusters"
        />
        {namespaceScoped && (
          <Select
            label="Namespace"
            data={[
              { value: ALL_NS, label: 'All namespaces' },
              ...(namespaces ?? []).map((n) => ({ value: n, label: n })),
            ]}
            value={namespace}
            onChange={(v) => setParam('namespace', v ?? ALL_NS)}
            searchable
            disabled={!ready}
            w={240}
          />
        )}
      </Group>

      {clusters.length === 0 && !clustersLoading ? (
        <EmptyState
          icon={IconServer2}
          title="No clusters yet"
          description="Create a cluster first - once it is Ready its networking shows up here."
          action={
            <Anchor component={Link} to="/clusters/new" mt="sm">
              New cluster
            </Anchor>
          }
        />
      ) : cluster && !ready ? (
        <Alert color="yellow" icon={<IconAlertTriangle size={18} />} title={`${cluster.name} is not Ready`}>
          Networking is available once the cluster reaches <b>Ready</b> (currently{' '}
          <b>{cluster.phase}</b>). Watch it converge on its{' '}
          <Anchor component={Link} to={`/clusters/${cluster.id}`}>
            cluster page
          </Anchor>
          .
        </Alert>
      ) : (
        <Tabs
          value={tab}
          onChange={(v) => {
            if (!v) return;
            // Drop the search term when switching tabs: the tables search different things, so
            // carrying it over would greet the new tab with an empty state for a query never typed
            // there (same reasoning as the Storage page).
            setParams((p) => {
              const next = new URLSearchParams(p);
              next.set('tab', v);
              next.delete('q');
              return next;
            });
          }}
          keepMounted={false}
          variant="outline"
        >
          <Tabs.List mb="md">
            <Tabs.Tab value="overview">Overview</Tabs.Tab>
            <Tabs.Tab value="services">Services</Tabs.Tab>
            <Tabs.Tab value="gateways">Gateways</Tabs.Tab>
            <Tabs.Tab value="routes">Routes</Tabs.Tab>
          </Tabs.List>

          <Tabs.Panel value="overview">
            <OverviewTab
              clusterId={clusterId}
              enabled={ready && tab === 'overview'}
              onSelectRoute={(_kind, ns, name) => {
                setPendingRoute({ namespace: ns, name });
                setParams((p) => {
                  const next = new URLSearchParams(p);
                  next.set('tab', 'routes');
                  next.delete('namespace');
                  next.delete('q');
                  return next;
                });
              }}
            />
          </Tabs.Panel>

          <Tabs.Panel value="services">
            <ServicesTab
              clusterId={clusterId}
              namespace={namespace}
              q={q}
              onSearch={(v) => setParam('q', v)}
              enabled={ready && tab === 'services'}
            />
          </Tabs.Panel>

          <Tabs.Panel value="gateways">
            <GatewaysTab
              clusterId={clusterId}
              q={q}
              onSearch={(v) => setParam('q', v)}
              enabled={ready && tab === 'gateways'}
            />
          </Tabs.Panel>

          <Tabs.Panel value="routes">
            <RoutesTab
              clusterId={clusterId}
              namespace={namespace}
              q={q}
              onSearch={(v) => setParam('q', v)}
              enabled={ready && tab === 'routes'}
              open={openRoute}
              setOpen={setOpenRoute}
              pending={pendingRoute}
              clearPending={() => setPendingRoute(null)}
            />
          </Tabs.Panel>
        </Tabs>
      )}
    </>
  );
}

// ---- Overview ----------------------------------------------------------------

function OverviewTab({
  clusterId,
  enabled,
  onSelectRoute,
}: {
  clusterId: string | undefined;
  enabled: boolean;
  onSelectRoute: (kind: string, namespace: string, name: string) => void;
}) {
  const { data: overview, isLoading, error } = useNetworkOverview(clusterId, enabled);

  if (error) {
    return (
      <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Could not load networking">
        {error instanceof ApiError ? error.message : String(error)}
      </Alert>
    );
  }
  if (isLoading && !overview) return <TableSkeleton />;
  if (!overview) return null;
  return (
    <NetworkingOverview overview={overview} clusterId={clusterId} onSelectRoute={onSelectRoute} />
  );
}

// ---- Services ----------------------------------------------------------------

function ServicesTab({
  clusterId,
  namespace,
  q,
  onSearch,
  enabled,
}: {
  clusterId: string | undefined;
  namespace: string;
  q: string;
  onSearch: (v: string) => void;
  enabled: boolean;
}) {
  const { data: services, isLoading, error } = useServices(clusterId, namespace, enabled);
  const [open, setOpen] = useState<ServiceSummary | null>(null);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = services ?? [];
    return needle
      ? list.filter(
          (s) =>
            s.name.toLowerCase().includes(needle) || s.namespace.toLowerCase().includes(needle),
        )
      : list;
  }, [services, q]);

  const external = rows.filter((s) => (s.external_ips ?? []).length > 0).length;

  return (
    <Card padding={0} radius="md">
      <Group p="sm" justify="space-between" wrap="wrap">
        <Text size="sm" c="dimmed">
          {rows.length} service{rows.length === 1 ? '' : 's'}
          {external > 0 && <> · {external} externally reachable</>}
        </Text>
        <TextInput
          placeholder="Search services…"
          leftSection={<IconSearch size={16} />}
          value={q}
          onChange={(e) => onSearch(e.currentTarget.value)}
          w={{ base: '100%', xs: 260 }}
        />
      </Group>

      {error ? (
        <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load services">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !services ? (
        <TableSkeleton />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={IconAffiliate}
          title="No services found"
          description={q ? 'No services match the current filters.' : 'This namespace has no services.'}
        />
      ) : (
        <Table.ScrollContainer minWidth={860}>
          <Table verticalSpacing="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Type</Table.Th>
                <Table.Th>Namespace</Table.Th>
                <Table.Th>Cluster IP</Table.Th>
                <Table.Th>External</Table.Th>
                <Table.Th>Ports</Table.Th>
                <Table.Th>Endpoints</Table.Th>
                <Table.Th>Age</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((s) => (
                <Table.Tr
                  key={`${s.namespace}/${s.name}`}
                  style={{ cursor: 'pointer' }}
                  onClick={() => setOpen(s)}
                >
                  <Table.Td style={{ maxWidth: 300 }}>
                    <Anchor fw={600} onClick={(e) => e.preventDefault()} style={{ wordBreak: 'break-all' }}>
                      {s.name}
                    </Anchor>
                  </Table.Td>
                  <Table.Td style={{ whiteSpace: 'nowrap' }}>
                    <Tooltip label={serviceTypeLabel(s.type)} multiline w={260}>
                      <Badge color={serviceTypeColor(s.type)} variant="dot" size="sm">
                        {s.type}
                      </Badge>
                    </Tooltip>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {s.namespace}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace">
                      {s.cluster_ip || 'None'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace" fw={(s.external_ips ?? []).length ? 600 : 400}>
                      {(s.external_ips ?? []).join(', ') || '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace">
                      {formatPorts(s.ports)}
                    </Text>
                  </Table.Td>
                  {/* Zero endpoints on a Service with a selector is the "nothing is backing this"
                      signal, so it is called out rather than rendered as a bland 0. */}
                  <Table.Td>
                    <Text size="sm" c={s.endpoints === 0 && s.selector ? 'yellow.7' : undefined}>
                      {s.endpoints}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
                      {relative(s.created_at)}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <ServiceDrawer
        clusterId={clusterId}
        service={open}
        opened={!!open}
        onClose={() => setOpen(null)}
      />
    </Card>
  );
}

// ---- Gateways ----------------------------------------------------------------

function GatewaysTab({
  clusterId,
  q,
  onSearch,
  enabled,
}: {
  clusterId: string | undefined;
  q: string;
  onSearch: (v: string) => void;
  enabled: boolean;
}) {
  const { data: gateways, isLoading, error } = useGateways(clusterId, enabled);
  const [open, setOpen] = useState<GatewaySummary | null>(null);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = gateways ?? [];
    return needle ? list.filter((g) => g.name.toLowerCase().includes(needle)) : list;
  }, [gateways, q]);

  return (
    <Card padding={0} radius="md">
      <Group p="sm" justify="space-between" wrap="wrap">
        <Text size="sm" c="dimmed">
          {rows.length} gateway{rows.length === 1 ? '' : 's'}
        </Text>
        <TextInput
          placeholder="Search gateways…"
          leftSection={<IconSearch size={16} />}
          value={q}
          onChange={(e) => onSearch(e.currentTarget.value)}
          w={{ base: '100%', xs: 260 }}
        />
      </Group>

      {error ? (
        <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load gateways">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !gateways ? (
        <TableSkeleton />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={IconWorld}
          title="No gateways"
          description={
            q
              ? 'No gateways match the current filters.'
              : 'This cluster has no Gateway API gateways - the envoy-gateway add-on installs the default one.'
          }
        />
      ) : (
        <Table.ScrollContainer minWidth={800}>
          <Table verticalSpacing="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Namespace</Table.Th>
                <Table.Th>Class</Table.Th>
                <Table.Th>Address</Table.Th>
                <Table.Th>Listeners</Table.Th>
                <Table.Th>Routes</Table.Th>
                <Table.Th>Status</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((g) => (
                <Table.Tr
                  key={`${g.namespace}/${g.name}`}
                  style={{ cursor: 'pointer' }}
                  onClick={() => setOpen(g)}
                >
                  <Table.Td>
                    <Group gap="xs" wrap="nowrap">
                      <Anchor fw={600} onClick={(e) => e.preventDefault()} style={{ wordBreak: 'break-all' }}>
                        {g.name}
                      </Anchor>
                      {g.is_default && (
                        <Tooltip label="The gateway this platform creates for every cluster" withArrow>
                          <Badge color="blue" variant="light" size="xs">
                            default
                          </Badge>
                        </Tooltip>
                      )}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {g.namespace}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace">
                      {g.class}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace" fw={600}>
                      {(g.addresses ?? []).join(', ') || '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      {(g.listeners ?? []).map((l) => (
                        <Badge key={l.name} variant="outline" color="gray" size="sm" radius="sm">
                          {l.protocol}:{l.port}
                        </Badge>
                      ))}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    {(g.listeners ?? []).reduce((n, l) => Math.max(n, l.attached_routes), 0)}
                  </Table.Td>
                  <Table.Td style={{ whiteSpace: 'nowrap' }}>
                    <Tooltip label={g.status || 'Programmed and serving'} withArrow>
                      <Badge color={readyColor(g.programmed)} variant="dot" size="sm">
                        {g.programmed ? 'Programmed' : 'Pending'}
                      </Badge>
                    </Tooltip>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <GatewayDrawer
        clusterId={clusterId}
        gateway={open}
        opened={!!open}
        onClose={() => setOpen(null)}
      />
    </Card>
  );
}

// ---- Routes ------------------------------------------------------------------

function RoutesTab({
  clusterId,
  namespace,
  q,
  onSearch,
  enabled,
  open,
  setOpen,
  pending,
  clearPending,
}: {
  clusterId: string | undefined;
  namespace: string;
  q: string;
  onSearch: (v: string) => void;
  enabled: boolean;
  open: RouteSummary | null;
  setOpen: (r: RouteSummary | null) => void;
  pending: { namespace: string; name: string } | null;
  clearPending: () => void;
}) {
  const { data: routes, isLoading, error } = useRoutes(clusterId, namespace, enabled);

  // A route named by the Overview's exposed-apps table opens as soon as the list that contains it
  // has loaded - the click happens on a tab whose data doesn't exist yet, so it can't be resolved
  // there.
  useEffect(() => {
    if (!pending || !routes) return;
    const match = routes.find((r) => r.namespace === pending.namespace && r.name === pending.name);
    if (match) setOpen(match);
    clearPending();
  }, [pending, routes, setOpen, clearPending]);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = routes ?? [];
    return needle
      ? list.filter(
          (r) =>
            r.name.toLowerCase().includes(needle) ||
            (r.hostnames ?? []).some((h) => h.toLowerCase().includes(needle)),
        )
      : list;
  }, [routes, q]);

  return (
    <Card padding={0} radius="md">
      <Group p="sm" justify="space-between" wrap="wrap">
        <Text size="sm" c="dimmed">
          {rows.length} route{rows.length === 1 ? '' : 's'}
        </Text>
        <TextInput
          placeholder="Search routes and hostnames…"
          leftSection={<IconSearch size={16} />}
          value={q}
          onChange={(e) => onSearch(e.currentTarget.value)}
          w={{ base: '100%', xs: 300 }}
        />
      </Group>

      {error ? (
        <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load routes">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !routes ? (
        <TableSkeleton />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={IconRoute}
          title="No routes"
          description={
            q
              ? 'No routes match the current filters.'
              : 'Nothing is published through a gateway yet. Attach an HTTPRoute to the default gateway to expose an app.'
          }
        />
      ) : (
        <Table.ScrollContainer minWidth={900}>
          <Table verticalSpacing="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Kind</Table.Th>
                <Table.Th>Namespace</Table.Th>
                <Table.Th>Hostnames</Table.Th>
                <Table.Th>Backends</Table.Th>
                <Table.Th>Gateway</Table.Th>
                <Table.Th>Status</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((r) => (
                <Table.Tr
                  key={`${r.kind}/${r.namespace}/${r.name}`}
                  style={{ cursor: 'pointer' }}
                  onClick={() => setOpen(r)}
                >
                  <Table.Td style={{ maxWidth: 240 }}>
                    <Anchor fw={600} onClick={(e) => e.preventDefault()} style={{ wordBreak: 'break-all' }}>
                      {r.name}
                    </Anchor>
                  </Table.Td>
                  <Table.Td style={{ whiteSpace: 'nowrap' }}>
                    <Badge variant="outline" color="gray" size="sm" radius="sm">
                      {routeKindLabel(r.kind)}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {r.namespace}
                    </Text>
                  </Table.Td>
                  <Table.Td style={{ maxWidth: 280 }}>
                    <Text size="sm" ff="monospace" style={{ wordBreak: 'break-all' }}>
                      {(r.hostnames ?? []).join(', ') || '*'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace" lineClamp={1} maw={200}>
                      {backendsText(r).join(', ') || '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed" lineClamp={1} maw={200}>
                      {(r.parent_refs ?? []).map((p) => p.name).join(', ') || '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td style={{ whiteSpace: 'nowrap' }}>
                    <Tooltip label={r.status || 'Accepted by its gateway'} withArrow>
                      <Badge color={readyColor(r.accepted)} variant="dot" size="sm">
                        {r.accepted ? 'Accepted' : 'Pending'}
                      </Badge>
                    </Tooltip>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <RouteDrawer
        clusterId={clusterId}
        route={open}
        opened={!!open}
        onClose={() => setOpen(null)}
      />
    </Card>
  );
}

function TableSkeleton() {
  return (
    <div style={{ padding: 16 }}>
      {[0, 1, 2, 3].map((i) => (
        <Skeleton key={i} height={40} mb={8} radius="sm" />
      ))}
    </div>
  );
}
