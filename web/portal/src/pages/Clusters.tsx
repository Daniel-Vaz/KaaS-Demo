import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router';
import {
  Group,
  Title,
  Button,
  TextInput,
  Table,
  Text,
  Badge,
  ActionIcon,
  Card,
  Skeleton,
  Tooltip,
  Anchor,
} from '@mantine/core';
import { modals } from '@mantine/modals';
import {
  IconPlus,
  IconSearch,
  IconTrash,
  IconServer2,
  IconChevronRight,
  IconShieldCheck,
} from '@tabler/icons-react';
import { useCatalog, useClusters, useDeleteCluster } from '../lib/queries';
import { useAuth } from '../lib/auth';
import { ClusterStatusBadge } from '../components/ClusterStatusBadge';
import { ProviderBadge } from '../components/ProviderBadge';
import { EmptyState } from '../components/EmptyState';
import {
  activeClusters,
  canManageCluster,
  controlPlaneCount,
  isHA,
  provisionedNodeCount,
  desiredNodeCount,
  clusterProvider,
} from '../lib/cluster';
import { relative } from '../lib/format';
import type { Cluster } from '../lib/types';

export function Clusters() {
  const navigate = useNavigate();
  const { data, isLoading } = useClusters();
  const { data: catalog } = useCatalog();
  const del = useDeleteCluster();
  const [q, setQ] = useState('');

  // Admins and group members can see clusters owned by someone else, so show an Owner column -
  // the API already resolves owner_username on every cluster the caller can see (see api.go).
  const { user } = useAuth();
  const showOwner = !!user?.is_admin || (user?.memberships?.length ?? 0) > 0;

  const clusters = useMemo(() => {
    const list = activeClusters(data ?? []);
    const needle = q.trim().toLowerCase();
    return needle ? list.filter((c) => c.name.toLowerCase().includes(needle)) : list;
  }, [data, q]);

  // Show the infrastructure whenever the deployment OFFERS a choice - not merely when the current
  // list happens to be mixed. On a multi-provider platform "which infrastructure is this on" is a
  // property of every cluster, and hiding the column until a second provider appears would mean
  // the answer blinks in and out as clusters come and go.
  const showInfra = useMemo(
    () => (catalog?.providers?.length ?? 1) > 1 || new Set(clusters.map(clusterProvider)).size > 1,
    [catalog, clusters],
  );

  const confirmDelete = (c: Cluster) =>
    modals.openConfirmModal({
      title: `Delete ${c.name}?`,
      centered: true,
      children: (
        <Text size="sm">
          This tears down the cluster and all of its VMs. The reconciler will move it to{' '}
          <b>Deleting</b> and remove it. This cannot be undone.
        </Text>
      ),
      labels: { confirm: 'Delete cluster', cancel: 'Cancel' },
      confirmProps: { color: 'red' },
      onConfirm: () => del.mutate(c.id),
    });

  return (
    <>
      <Group justify="space-between" mb="lg">
        <div>
          <Title order={2}>Clusters</Title>
          <Text c="dimmed" size="sm">
            Request and manage Kubernetes clusters on the local control plane.
          </Text>
        </div>
        <Button component={Link} to="/clusters/new" leftSection={<IconPlus size={18} />}>
          New cluster
        </Button>
      </Group>

      <Card padding={0} radius="md">
        <Group p="sm" justify="space-between">
          <TextInput
            placeholder="Search clusters…"
            leftSection={<IconSearch size={16} />}
            value={q}
            onChange={(e) => setQ(e.currentTarget.value)}
            w={{ base: '100%', xs: 280 }}
          />
          {data && (
            <Text size="sm" c="dimmed" visibleFrom="xs">
              {clusters.length} cluster{clusters.length === 1 ? '' : 's'}
            </Text>
          )}
        </Group>

        {isLoading ? (
          <div style={{ padding: 16 }}>
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} height={44} mb={8} radius="sm" />
            ))}
          </div>
        ) : clusters.length === 0 ? (
          <EmptyState
            icon={IconServer2}
            title={q ? 'No clusters match your search' : 'No clusters yet'}
            description={q ? undefined : 'Create your first cluster to see it provision live.'}
            action={
              !q ? (
                <Button component={Link} to="/clusters/new" variant="light" mt="sm" leftSection={<IconPlus size={16} />}>
                  New cluster
                </Button>
              ) : undefined
            }
          />
        ) : (
          <Table.ScrollContainer minWidth={760}>
            <Table verticalSpacing="sm" highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Name</Table.Th>
                  {showOwner && <Table.Th>Owner</Table.Th>}
                  <Table.Th>Status</Table.Th>
                  <Table.Th>Kubernetes</Table.Th>
                  {showInfra && <Table.Th>Infrastructure</Table.Th>}
                  <Table.Th>Control plane</Table.Th>
                  <Table.Th>Nodes</Table.Th>
                  <Table.Th>Size</Table.Th>
                  <Table.Th>Created</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {clusters.map((c) => (
                  <Table.Tr
                    key={c.id}
                    style={{ cursor: 'pointer' }}
                    onClick={() => navigate(`/clusters/${c.id}`)}
                  >
                    <Table.Td>
                      <Anchor component={Link} to={`/clusters/${c.id}`} fw={600} onClick={(e) => e.stopPropagation()}>
                        {c.name}
                      </Anchor>
                    </Table.Td>
                    {showOwner && (
                      <Table.Td>
                        <Text size="sm" c="dimmed">
                          {c.owner_username}
                        </Text>
                      </Table.Td>
                    )}
                    <Table.Td>
                      <ClusterStatusBadge phase={c.phase} />
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{c.k8s_version}</Text>
                      <Text size="xs" c="dimmed">
                        {c.bundle}
                      </Text>
                    </Table.Td>
                    {showInfra && (
                      <Table.Td>
                        <ProviderBadge cluster={c} />
                      </Table.Td>
                    )}
                    <Table.Td>
                      {isHA(c) ? (
                        <Badge color="grape" variant="light" size="sm" leftSection={<IconShieldCheck size={12} />}>
                          HA · {controlPlaneCount(c)}
                        </Badge>
                      ) : (
                        <Badge color="gray" variant="light" size="sm">
                          single
                        </Badge>
                      )}
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">
                        {provisionedNodeCount(c)}
                        <Text span c="dimmed">
                          {' '}
                          / {desiredNodeCount(c)}
                        </Text>
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Badge variant="default" size="sm" radius="sm">
                        {c.size}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" c="dimmed">
                        {relative(c.created_at)}
                      </Text>
                    </Table.Td>
                    <Table.Td onClick={(e) => e.stopPropagation()}>
                      <Group gap={4} justify="flex-end" wrap="nowrap">
                        {/* Delete is a write action - hidden for read-only group members (they can
                            still open the cluster to view it). The server enforces this too. */}
                        {canManageCluster(c, user) && (
                          <Tooltip label="Delete">
                            <ActionIcon
                              variant="subtle"
                              color="red"
                              onClick={() => confirmDelete(c)}
                              disabled={c.phase === 'Deleting'}
                            >
                              <IconTrash size={16} />
                            </ActionIcon>
                          </Tooltip>
                        )}
                        <ActionIcon variant="subtle" color="gray" component={Link} to={`/clusters/${c.id}`}>
                          <IconChevronRight size={16} />
                        </ActionIcon>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </Card>
    </>
  );
}
