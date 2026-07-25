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
  Button,
} from '@mantine/core';
import {
  IconSearch,
  IconDatabase,
  IconAlertTriangle,
  IconServer2,
  IconExternalLink,
} from '@tabler/icons-react';
import { useClusters, useNamespaces, usePVCs, useStorageApps, useStorageClasses } from '../lib/queries';
import { ApiError } from '../lib/api';
import { EmptyState } from '../components/EmptyState';
import { activeClusters, clusterUsable } from '../lib/cluster';
import { relative } from '../lib/format';
import { pvcStatusColor, accessModeLabel, totalCapacity } from '../lib/storage';
import { PVCDrawer } from '../components/storage/PVCDrawer';
import { StorageClassDrawer } from '../components/storage/StorageClassDrawer';
import type { PVCSummary, StorageClass } from '../lib/types';

// All-namespaces sentinel for the namespace <Select> (empty string maps to "all" on the API).
const ALL_NS = '';

export function Storage() {
  const [params, setParams] = useSearchParams();
  const { data: clustersRaw, isLoading: clustersLoading } = useClusters();
  const clusters = useMemo(() => activeClusters(clustersRaw ?? []), [clustersRaw]);

  // Selected cluster: from the URL, else the first usable cluster, else the first - mirroring the
  // Workloads/Security pages' resolution.
  const urlCluster = params.get('cluster') ?? '';
  const selected = clusters.find((c) => c.id === urlCluster);
  const fallback = clusters.find(clusterUsable) ?? clusters[0];
  const cluster = selected ?? fallback;
  const clusterId = cluster?.id;
  const ready = !!cluster && clusterUsable(cluster);

  // Keep the URL in sync with the resolved cluster so the page is deep-linkable and stable on reload.
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
  const tab = params.get('tab') || 'claims';
  const q = params.get('q') ?? '';

  const setParam = (key: string, value: string) =>
    setParams((p) => {
      const next = new URLSearchParams(p);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });

  const { data: namespaces } = useNamespaces(clusterId, ready);

  // "Open UI" - Longhorn's own console, reverse-proxied per cluster like the Monitoring page's links
  // (see internal/tunnel). It is write_scoped: longhorn-ui ships no auth of its own and it deletes
  // volumes, so a read-role group-mate must not see the link (the API's 403 is the real gate).
  const { data: storageApps } = useStorageApps(clusterId);
  const visibleApps = useMemo(
    () =>
      (storageApps?.apps ?? []).filter(
        (a) => !a.write_scoped || (cluster?.can_manage ?? false),
      ),
    [storageApps?.apps, cluster?.can_manage],
  );

  return (
    <>
      <Group justify="space-between" mb="lg" align="flex-start">
        <div>
          <Title order={2}>Storage</Title>
          <Text c="dimmed" size="sm">
            The persistent volume claims running in a cluster, and the storage classes they are
            provisioned from.
          </Text>
        </div>
      </Group>

      {/* Cluster + namespace pickers (GKE-style). The namespace picker only bears on claims -
          StorageClasses are cluster-scoped - so it is hidden on the Classes tab. */}
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
        {tab === 'claims' && (
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

      {ready && storageApps?.enabled && clusterId && visibleApps.length > 0 && (
        <Group mb="md" gap="xs" align="center">
          <Text size="sm" c="dimmed">
            Open UI:
          </Text>
          {visibleApps.map((appLink) => (
            <Button
              key={appLink.id}
              component="a"
              href={`/api/clusters/${clusterId}/proxy/${appLink.id}/`}
              target="_blank"
              rel="noopener noreferrer"
              variant="light"
              size="xs"
              rightSection={<IconExternalLink size={14} />}
            >
              {appLink.name}
            </Button>
          ))}
        </Group>
      )}

      {clusters.length === 0 && !clustersLoading ? (
        <EmptyState
          icon={IconServer2}
          title="No clusters yet"
          description="Create a cluster first - once it is Ready its storage shows up here."
          action={
            <Anchor component={Link} to="/clusters/new" mt="sm">
              New cluster
            </Anchor>
          }
        />
      ) : cluster && !ready ? (
        <Alert color="yellow" icon={<IconAlertTriangle size={18} />} title={`${cluster.name} is not Ready`}>
          Storage is available once the cluster reaches <b>Ready</b> (currently <b>{cluster.phase}</b>).
          Watch it converge on its{' '}
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
            // Drop the search term when switching tabs: the two tables search different things (claim
            // names vs. class names/provisioners), so carrying it over would greet the new tab with a
            // "nothing matches" empty state for a query the user never typed there.
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
            <Tabs.Tab value="claims">Volume claims</Tabs.Tab>
            <Tabs.Tab value="classes">Storage classes</Tabs.Tab>
          </Tabs.List>

          <Tabs.Panel value="claims">
            <ClaimsTab
              clusterId={clusterId}
              namespace={namespace}
              q={q}
              onSearch={(v) => setParam('q', v)}
              enabled={ready && tab === 'claims'}
            />
          </Tabs.Panel>

          <Tabs.Panel value="classes">
            <ClassesTab
              clusterId={clusterId}
              q={q}
              onSearch={(v) => setParam('q', v)}
              enabled={ready && tab === 'classes'}
            />
          </Tabs.Panel>
        </Tabs>
      )}
    </>
  );
}

// ---- Claims ------------------------------------------------------------------

function ClaimsTab({
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
  const { data: pvcs, isLoading, error } = usePVCs(clusterId, namespace, enabled);
  const [openPVC, setOpenPVC] = useState<PVCSummary | null>(null);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = pvcs ?? [];
    return needle ? list.filter((p) => p.name.toLowerCase().includes(needle)) : list;
  }, [pvcs, q]);

  return (
    <Card padding={0} radius="md">
      <Group p="sm" justify="space-between" wrap="wrap">
        {/* The total is over the rows in view, so it answers the question the filters just asked. */}
        <Text size="sm" c="dimmed">
          {rows.length} claim{rows.length === 1 ? '' : 's'}
          {rows.length > 0 && <> · {totalCapacity(rows)} provisioned</>}
        </Text>
        <TextInput
          placeholder="Search claims…"
          leftSection={<IconSearch size={16} />}
          value={q}
          onChange={(e) => onSearch(e.currentTarget.value)}
          w={{ base: '100%', xs: 260 }}
        />
      </Group>

      {error ? (
        <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load volume claims">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !pvcs ? (
        <TableSkeleton />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={IconDatabase}
          title="No volume claims found"
          description={
            q
              ? 'No claims match the current filters.'
              : 'Nothing in this cluster has requested persistent storage yet.'
          }
        />
      ) : (
        <Table.ScrollContainer minWidth={880}>
          <Table verticalSpacing="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Status</Table.Th>
                <Table.Th>Namespace</Table.Th>
                <Table.Th>Capacity</Table.Th>
                <Table.Th>Access</Table.Th>
                <Table.Th>Storage class</Table.Th>
                <Table.Th>Volume</Table.Th>
                <Table.Th>Age</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((p) => (
                <Table.Tr
                  key={`${p.namespace}/${p.name}`}
                  style={{ cursor: 'pointer' }}
                  onClick={() => setOpenPVC(p)}
                >
                  {/* Claim names get long (a StatefulSet volumeClaimTemplate's name is the template +
                      the pod's), so the name is capped and wraps rather than starving the columns
                      after it - the status badge in particular must never truncate to "Bou…". */}
                  <Table.Td style={{ maxWidth: 320 }}>
                    <Anchor fw={600} onClick={(e) => e.preventDefault()} style={{ wordBreak: 'break-all' }}>
                      {p.name}
                    </Anchor>
                  </Table.Td>
                  <Table.Td style={{ whiteSpace: 'nowrap' }}>
                    <Badge color={pvcStatusColor(p.status)} variant="dot" size="sm">
                      {p.status}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {p.namespace}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" fw={600}>
                      {p.capacity || p.requested || '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      {(p.access_modes ?? []).map((m) => (
                        <Tooltip key={m} label={accessModeLabel(m)} multiline w={260}>
                          <Badge variant="outline" color="gray" size="sm" radius="sm">
                            {m}
                          </Badge>
                        </Tooltip>
                      ))}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace">
                      {p.storage_class || '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" ff="monospace" c="dimmed" lineClamp={1} maw={200}>
                      {p.volume || '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
                      {relative(p.created_at)}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <PVCDrawer
        clusterId={clusterId}
        pvc={openPVC}
        opened={!!openPVC}
        onClose={() => setOpenPVC(null)}
      />
    </Card>
  );
}

// ---- Classes -----------------------------------------------------------------

function ClassesTab({
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
  const { data: classes, isLoading, error } = useStorageClasses(clusterId, enabled);
  const [openClass, setOpenClass] = useState<StorageClass | null>(null);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = classes ?? [];
    return needle
      ? list.filter(
          (c) => c.name.toLowerCase().includes(needle) || c.provisioner.toLowerCase().includes(needle),
        )
      : list;
  }, [classes, q]);

  return (
    <Card padding={0} radius="md">
      <Group p="sm" justify="space-between" wrap="wrap">
        <Text size="sm" c="dimmed">
          {rows.length} storage class{rows.length === 1 ? '' : 'es'}
        </Text>
        <TextInput
          placeholder="Search classes…"
          leftSection={<IconSearch size={16} />}
          value={q}
          onChange={(e) => onSearch(e.currentTarget.value)}
          w={{ base: '100%', xs: 260 }}
        />
      </Group>

      {error ? (
        <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load storage classes">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !classes ? (
        <TableSkeleton />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={IconDatabase}
          title="No storage classes found"
          description={
            q
              ? 'No classes match the current filters.'
              : 'This cluster has no storage classes, so claims cannot be dynamically provisioned.'
          }
        />
      ) : (
        <Table.ScrollContainer minWidth={820}>
          <Table verticalSpacing="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Provisioner</Table.Th>
                <Table.Th>Reclaim policy</Table.Th>
                <Table.Th>Binding mode</Table.Th>
                <Table.Th>Expansion</Table.Th>
                <Table.Th>Age</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((c) => (
                <Table.Tr key={c.name} style={{ cursor: 'pointer' }} onClick={() => setOpenClass(c)}>
                  <Table.Td>
                    <Group gap="xs" wrap="nowrap">
                      <Anchor fw={600} onClick={(e) => e.preventDefault()}>
                        {c.name}
                      </Anchor>
                      {c.is_default && (
                        <Badge color="blue" variant="light" size="xs">
                          default
                        </Badge>
                      )}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace">
                      {c.provisioner}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{c.reclaim_policy || '-'}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{c.volume_binding_mode || '-'}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge color={c.allow_expansion ? 'green' : 'gray'} variant="light" size="sm">
                      {c.allow_expansion ? 'Allowed' : 'No'}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
                      {relative(c.created_at)}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <StorageClassDrawer
        clusterId={clusterId}
        storageClass={openClass}
        opened={!!openClass}
        onClose={() => setOpenClass(null)}
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
