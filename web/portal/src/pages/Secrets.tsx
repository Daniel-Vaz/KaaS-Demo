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
  Skeleton,
  Anchor,
  Alert,
  Button,
  Tooltip,
  Stack,
  Code,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import {
  IconSearch,
  IconKey,
  IconFileText,
  IconLock,
  IconAlertTriangle,
  IconServer2,
  IconExternalLink,
} from '@tabler/icons-react';
import { useClusters, useNamespaces, useConfigMaps, useSecrets } from '../lib/queries';
import { api, ApiError } from '../lib/api';
import { copyText } from '../lib/clipboard';
import { EmptyState } from '../components/EmptyState';
import { activeClusters, clusterUsable } from '../lib/cluster';
import { relative } from '../lib/format';
import { ConfigMapDrawer } from '../components/secrets/ConfigMapDrawer';
import { SecretDrawer } from '../components/secrets/SecretDrawer';
import type { ConfigMapSummary, SecretSummary } from '../lib/types';

// All-namespaces sentinel for the namespace <Select> (empty string maps to "all" on the API).
const ALL_NS = '';

export function Secrets() {
  const [params, setParams] = useSearchParams();
  const { data: clustersRaw, isLoading: clustersLoading } = useClusters();
  const clusters = useMemo(() => activeClusters(clustersRaw ?? []), [clustersRaw]);

  // Selected cluster: from the URL, else the first usable cluster, else the first - mirroring the
  // Workloads/Storage pages' resolution.
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
  const tab = params.get('tab') || 'secrets';
  const q = params.get('q') ?? '';

  const setParam = (key: string, value: string) =>
    setParams((p) => {
      const next = new URLSearchParams(p);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });

  const { data: namespaces } = useNamespaces(clusterId, ready);

  return (
    <>
      <Group justify="space-between" mb="lg" align="flex-start">
        <div>
          <Title order={2}>Secrets</Title>
          <Text c="dimmed" size="sm">
            The ConfigMaps and Secrets in a cluster. Secret values are redacted - their source of truth
            is HashiCorp Vault, synced in by the External Secrets Operator.
          </Text>
        </div>
        {ready && clusterId && <VaultButton clusterId={clusterId} wired={cluster?.vault_wired ?? false} />}
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
          description="Create a cluster first - once it is Ready its ConfigMaps and Secrets show up here."
          action={
            <Anchor component={Link} to="/clusters/new" mt="sm">
              New cluster
            </Anchor>
          }
        />
      ) : cluster && !ready ? (
        <Alert color="yellow" icon={<IconAlertTriangle size={18} />} title={`${cluster.name} is not Ready`}>
          ConfigMaps and Secrets are available once the cluster reaches <b>Ready</b> (currently{' '}
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
            // carrying it over would greet the new tab with a "nothing matches" for a query never typed.
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
            <Tabs.Tab value="secrets" leftSection={<IconLock size={14} />}>
              Secrets
            </Tabs.Tab>
            <Tabs.Tab value="configmaps" leftSection={<IconFileText size={14} />}>
              ConfigMaps
            </Tabs.Tab>
          </Tabs.List>

          <Tabs.Panel value="secrets">
            <SecretsTab
              clusterId={clusterId}
              namespace={namespace}
              q={q}
              onSearch={(v) => setParam('q', v)}
              enabled={ready && tab === 'secrets'}
            />
          </Tabs.Panel>

          <Tabs.Panel value="configmaps">
            <ConfigMapsTab
              clusterId={clusterId}
              namespace={namespace}
              q={q}
              onSearch={(v) => setParam('q', v)}
              enabled={ready && tab === 'configmaps'}
            />
          </Tabs.Panel>
        </Tabs>
      )}
    </>
  );
}

// VaultButton mints the "View in Vault" handoff, copies the short-lived token to the clipboard, and
// opens the Vault UI at the cluster's path. Only enabled once the cluster's Vault path is provisioned.
function VaultButton({ clusterId, wired }: { clusterId: string; wired: boolean }) {
  const [loading, setLoading] = useState(false);
  const open = async () => {
    setLoading(true);
    try {
      const s = await api.getVaultSession(clusterId);
      if (await copyText(s.token)) {
        notifications.show({
          color: 'violet',
          title: 'Vault token copied',
          message: 'A short-lived token was copied to your clipboard - paste it into the Vault "Token" field to sign in.',
        });
      } else {
        // No clipboard at all (an insecure origin whose browser also refuses the legacy path).
        // Show the token so the handoff still works by hand - it is short-lived and scoped to the
        // access this user already has, and the alternative is a button that does nothing.
        notifications.show({
          color: 'violet',
          title: 'Copy this Vault token',
          autoClose: false,
          message: (
            <Stack gap={4}>
              <Text size="sm">Your browser blocked the clipboard. Copy the token and paste it into the Vault &quot;Token&quot; field:</Text>
              <Code block style={{ wordBreak: 'break-all' }}>{s.token}</Code>
            </Stack>
          ),
        });
      }
      window.open(s.url, '_blank', 'noopener,noreferrer');
    } catch (e) {
      notifications.show({
        color: 'red',
        title: 'Could not open Vault',
        message: e instanceof ApiError ? e.message : String(e),
      });
    } finally {
      setLoading(false);
    }
  };
  const btn = (
    <Button
      variant="light"
      color="violet"
      leftSection={<IconKey size={16} />}
      rightSection={<IconExternalLink size={14} />}
      loading={loading}
      disabled={!wired}
      onClick={open}
    >
      View in Vault
    </Button>
  );
  return wired ? btn : (
    <Tooltip label="This cluster's Vault path is being provisioned (needs the external-secrets add-on).">
      <span>{btn}</span>
    </Tooltip>
  );
}

// ---- Secrets -----------------------------------------------------------------

function SecretsTab({
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
  const { data: secrets, isLoading, error } = useSecrets(clusterId, namespace, enabled);
  const [open, setOpen] = useState<SecretSummary | null>(null);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = secrets ?? [];
    return needle ? list.filter((s) => s.name.toLowerCase().includes(needle)) : list;
  }, [secrets, q]);

  return (
    <Card padding={0} radius="md">
      <Group p="sm" justify="space-between" wrap="wrap">
        <Text size="sm" c="dimmed">
          {rows.length} secret{rows.length === 1 ? '' : 's'}
        </Text>
        <TextInput
          placeholder="Search secrets…"
          leftSection={<IconSearch size={16} />}
          value={q}
          onChange={(e) => onSearch(e.currentTarget.value)}
          w={{ base: '100%', xs: 260 }}
        />
      </Group>

      {error ? (
        <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load secrets">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !secrets ? (
        <TableSkeleton />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={IconLock}
          title="No secrets found"
          description={q ? 'No secrets match the current filters.' : 'This namespace has no secrets.'}
        />
      ) : (
        <Table.ScrollContainer minWidth={820}>
          <Table verticalSpacing="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Type</Table.Th>
                <Table.Th>Namespace</Table.Th>
                <Table.Th>Keys</Table.Th>
                <Table.Th>Source</Table.Th>
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
                  <Table.Td style={{ maxWidth: 320 }}>
                    <Group gap="xs" wrap="nowrap">
                      <IconLock size={14} opacity={0.5} />
                      <Anchor fw={600} onClick={(e) => e.preventDefault()} style={{ wordBreak: 'break-all' }}>
                        {s.name}
                      </Anchor>
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" ff="monospace" c="dimmed">
                      {s.type}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {s.namespace}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{s.data_count}</Text>
                  </Table.Td>
                  <Table.Td>
                    {s.managed_by ? (
                      <Tooltip label={`Synced from Vault by ExternalSecret ${s.managed_by}`}>
                        <Badge color="violet" variant="light" size="sm" leftSection={<IconKey size={11} />}>
                          Vault
                        </Badge>
                      </Tooltip>
                    ) : (
                      <Text size="sm" c="dimmed">
                        -
                      </Text>
                    )}
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

      <SecretDrawer clusterId={clusterId} secret={open} opened={!!open} onClose={() => setOpen(null)} />
    </Card>
  );
}

// ---- ConfigMaps --------------------------------------------------------------

function ConfigMapsTab({
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
  const { data: cms, isLoading, error } = useConfigMaps(clusterId, namespace, enabled);
  const [open, setOpen] = useState<ConfigMapSummary | null>(null);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = cms ?? [];
    return needle ? list.filter((c) => c.name.toLowerCase().includes(needle)) : list;
  }, [cms, q]);

  return (
    <Card padding={0} radius="md">
      <Group p="sm" justify="space-between" wrap="wrap">
        <Text size="sm" c="dimmed">
          {rows.length} config map{rows.length === 1 ? '' : 's'}
        </Text>
        <TextInput
          placeholder="Search config maps…"
          leftSection={<IconSearch size={16} />}
          value={q}
          onChange={(e) => onSearch(e.currentTarget.value)}
          w={{ base: '100%', xs: 260 }}
        />
      </Group>

      {error ? (
        <Alert color="red" m="sm" icon={<IconAlertTriangle size={18} />} title="Could not load config maps">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !cms ? (
        <TableSkeleton />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={IconFileText}
          title="No config maps found"
          description={q ? 'No config maps match the current filters.' : 'This namespace has no config maps.'}
        />
      ) : (
        <Table.ScrollContainer minWidth={760}>
          <Table verticalSpacing="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Namespace</Table.Th>
                <Table.Th>Keys</Table.Th>
                <Table.Th>Age</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((c) => (
                <Table.Tr
                  key={`${c.namespace}/${c.name}`}
                  style={{ cursor: 'pointer' }}
                  onClick={() => setOpen(c)}
                >
                  <Table.Td style={{ maxWidth: 340 }}>
                    <Group gap="xs" wrap="nowrap">
                      <IconFileText size={14} opacity={0.5} />
                      <Anchor fw={600} onClick={(e) => e.preventDefault()} style={{ wordBreak: 'break-all' }}>
                        {c.name}
                      </Anchor>
                      {c.immutable && (
                        <Badge color="gray" variant="light" size="xs">
                          immutable
                        </Badge>
                      )}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {c.namespace}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{c.data_count}</Text>
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

      <ConfigMapDrawer clusterId={clusterId} configMap={open} opened={!!open} onClose={() => setOpen(null)} />
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
