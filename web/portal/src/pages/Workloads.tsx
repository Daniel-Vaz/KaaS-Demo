import { useEffect, useMemo, useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import {
  Group,
  Title,
  Text,
  Select,
  SegmentedControl,
  TextInput,
  Table,
  Badge,
  Card,
  Skeleton,
  Anchor,
  Alert,
  Pagination,
} from '@mantine/core';
import { IconSearch, IconStack2, IconAlertTriangle, IconServer2 } from '@tabler/icons-react';
import { useClusters, useNamespaces, useWorkloads } from '../lib/queries';
import { ApiError } from '../lib/api';
import { EmptyState } from '../components/EmptyState';
import { activeClusters, clusterUsable } from '../lib/cluster';
import { relative } from '../lib/format';
import { kindLabel, kindColor, statusColor } from '../lib/workloads';
import type { WorkloadKind, WorkloadSummary } from '../lib/types';
import { WORKLOAD_KINDS } from '../lib/types';

// All-namespaces sentinel for the namespace <Select> (empty string maps to "all" on the API).
const ALL_NS = '';

// Rows rendered per page. The API returns the whole workload list for a namespace, so a big cluster
// (hundreds/thousands of workloads across all namespaces) would otherwise mount every row at once;
// paging bounds the DOM regardless of cluster size.
const PAGE_SIZE = 50;

export function Workloads() {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const { data: clustersRaw, isLoading: clustersLoading } = useClusters();

  const clusters = useMemo(() => activeClusters(clustersRaw ?? []), [clustersRaw]);

  // Selected cluster: from the URL, else the first usable cluster, else the first cluster.
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
  const kindFilter = (params.get('kind') as WorkloadKind | 'all' | null) ?? 'all';
  const q = params.get('q') ?? '';

  const setParam = (key: string, value: string) =>
    setParams((p) => {
      const next = new URLSearchParams(p);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });

  const { data: namespaces } = useNamespaces(clusterId, ready);
  const { data: workloads, isLoading, error } = useWorkloads(clusterId, namespace, ready);

  const rows = useMemo(() => {
    let list = workloads ?? [];
    if (kindFilter !== 'all') list = list.filter((w) => w.kind === kindFilter);
    const needle = q.trim().toLowerCase();
    if (needle) list = list.filter((w) => w.name.toLowerCase().includes(needle));
    return list;
  }, [workloads, kindFilter, q]);

  // Client-side pagination over the filtered rows. Jump back to the first page whenever the result
  // set changes underneath us (cluster/namespace switch, a kind filter, or a search) so the user
  // isn't stranded on a now-empty page. `current` is additionally clamped because the list polls and
  // can shrink under a stale page number.
  const [page, setPage] = useState(1);
  useEffect(() => setPage(1), [clusterId, namespace, kindFilter, q]);
  const pageCount = Math.max(1, Math.ceil(rows.length / PAGE_SIZE));
  const current = Math.min(Math.max(page, 1), pageCount);
  const pageRows = useMemo(
    () => rows.slice((current - 1) * PAGE_SIZE, current * PAGE_SIZE),
    [rows, current],
  );
  const firstShown = rows.length === 0 ? 0 : (current - 1) * PAGE_SIZE + 1;
  const lastShown = Math.min(current * PAGE_SIZE, rows.length);

  const openWorkload = (w: WorkloadSummary) =>
    navigate(
      `/workloads/${clusterId}/${w.kind}/${encodeURIComponent(w.namespace)}/${encodeURIComponent(w.name)}`,
    );

  return (
    <>
      <Group justify="space-between" mb="lg" align="flex-start">
        <div>
          <Title order={2}>Workloads</Title>
          <Text c="dimmed" size="sm">
            Browse the Deployments, StatefulSets, DaemonSets, Jobs and CronJobs running in a cluster.
          </Text>
        </div>
      </Group>

      {/* Cluster + namespace pickers (GKE-style). */}
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
      </Group>

      {clusters.length === 0 && !clustersLoading ? (
        <EmptyState
          icon={IconServer2}
          title="No clusters yet"
          description="Create a cluster first - once it is Ready its workloads show up here."
          action={
            <Anchor component={Link} to="/clusters/new" mt="sm">
              New cluster
            </Anchor>
          }
        />
      ) : cluster && !ready ? (
        <Alert color="yellow" icon={<IconAlertTriangle size={18} />} title={`${cluster.name} is not Ready`}>
          Workloads are available once the cluster reaches <b>Ready</b> (currently{' '}
          <b>{cluster.phase}</b>). Watch it converge on its{' '}
          <Anchor component={Link} to={`/clusters/${cluster.id}`}>
            cluster page
          </Anchor>
          .
        </Alert>
      ) : (
        <Card padding={0} radius="md">
          <Group p="sm" justify="space-between" wrap="wrap">
            <SegmentedControl
              size="sm"
              value={kindFilter}
              onChange={(v) => setParam('kind', v === 'all' ? '' : v)}
              data={[
                { value: 'all', label: 'All' },
                ...WORKLOAD_KINDS.map((k) => ({ value: k, label: `${kindLabel(k)}s` })),
              ]}
            />
            <TextInput
              placeholder="Search workloads…"
              leftSection={<IconSearch size={16} />}
              value={q}
              onChange={(e) => setParam('q', e.currentTarget.value)}
              w={{ base: '100%', xs: 260 }}
            />
          </Group>

          {error ? (
            <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load workloads">
              {error instanceof ApiError ? error.message : String(error)}
            </Alert>
          ) : isLoading && !workloads ? (
            <div style={{ padding: 16 }}>
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} height={40} mb={8} radius="sm" />
              ))}
            </div>
          ) : rows.length === 0 ? (
            <EmptyState
              icon={IconStack2}
              title="No workloads found"
              description={
                q || kindFilter !== 'all'
                  ? 'No workloads match the current filters.'
                  : 'This namespace has no workloads.'
              }
            />
          ) : (
            <>
            <Table.ScrollContainer minWidth={820}>
              <Table verticalSpacing="sm" highlightOnHover>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Name</Table.Th>
                    <Table.Th>Type</Table.Th>
                    <Table.Th>Namespace</Table.Th>
                    <Table.Th>Pods</Table.Th>
                    <Table.Th>Status</Table.Th>
                    <Table.Th>Images</Table.Th>
                    <Table.Th>Age</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {pageRows.map((w) => (
                    <Table.Tr
                      key={`${w.kind}/${w.namespace}/${w.name}`}
                      style={{ cursor: 'pointer' }}
                      onClick={() => openWorkload(w)}
                    >
                      <Table.Td>
                        <Anchor fw={600} onClick={(e) => e.preventDefault()}>
                          {w.name}
                        </Anchor>
                      </Table.Td>
                      <Table.Td>
                        <Badge color={kindColor(w.kind)} variant="light" size="sm">
                          {kindLabel(w.kind)}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" c="dimmed">
                          {w.namespace}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <PodsCell w={w} />
                      </Table.Td>
                      <Table.Td>
                        <Badge color={statusColor(w.status)} variant="dot" size="sm">
                          {w.status}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs" c="dimmed" lineClamp={2} maw={280}>
                          {(w.images ?? []).join(', ') || '-'}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" c="dimmed">
                          {relative(w.created_at)}
                        </Text>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
            {pageCount > 1 && (
              <Group p="sm" justify="space-between" wrap="wrap" gap="sm">
                <Text size="xs" c="dimmed">
                  Showing {firstShown}–{lastShown} of {rows.length}
                </Text>
                <Pagination total={pageCount} value={current} onChange={setPage} size="sm" />
              </Group>
            )}
            </>
          )}
        </Card>
      )}
    </>
  );
}

// PodsCell renders "ready/desired" for controllers, or the schedule for a CronJob.
function PodsCell({ w }: { w: WorkloadSummary }) {
  if (w.kind === 'cronjob') {
    return (
      <Text size="sm" ff="monospace">
        {w.schedule ?? '-'}
      </Text>
    );
  }
  const ok = w.ready_replicas >= w.desired_replicas;
  return (
    <Text size="sm" c={ok ? undefined : 'yellow.7'} fw={ok ? undefined : 600}>
      {w.ready_replicas}/{w.desired_replicas}
    </Text>
  );
}
