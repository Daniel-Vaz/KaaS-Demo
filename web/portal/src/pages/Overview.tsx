import { useMemo } from 'react';
import { Link } from 'react-router';
import {
  Group,
  Title,
  Text,
  SimpleGrid,
  Card,
  Button,
  Stack,
  Skeleton,
  Center,
  Badge,
  Anchor,
  UnstyledButton,
} from '@mantine/core';
import { DonutChart } from '@mantine/charts';
import {
  IconPlus,
  IconStack2,
  IconCircleCheck,
  IconLoader2,
  IconAlertTriangle,
  IconServer2,
} from '@tabler/icons-react';
import { useClusters, useCapacity, useCatalog } from '../lib/queries';
import { StatCard } from '../components/StatCard';
import { CapacityGauges } from '../components/CapacityGauges';
import { ClusterStatusBadge } from '../components/ClusterStatusBadge';
import { ProviderBadge } from '../components/ProviderBadge';
import { EmptyState } from '../components/EmptyState';
import { activeClusters, clusterProvider } from '../lib/cluster';
import { phaseMeta } from '../lib/phase';
import { relative } from '../lib/format';
import type { Cluster } from '../lib/types';

// Map a Mantine colour name to a CSS variable the charts can render.
const cssColor = (c: string) => `var(--mantine-color-${c}-6)`;

function phaseCounts(clusters: Cluster[]) {
  const ready = clusters.filter((c) => c.phase === 'Ready').length;
  const failed = clusters.filter((c) => c.phase === 'Failed').length;
  const deleting = clusters.filter((c) => c.phase === 'Deleting').length;
  const provisioning = clusters.length - ready - failed - deleting;
  return { ready, failed, deleting, provisioning };
}

export function Overview() {
  const { data: rawClusters, isLoading } = useClusters();
  const { data: cap } = useCapacity();
  const { data: catalog } = useCatalog();

  const clusters = useMemo(() => activeClusters(rawClusters ?? []), [rawClusters]);
  const counts = useMemo(() => phaseCounts(clusters), [clusters]);

  // With more than one infrastructure on offer, every cluster gets an infrastructure badge -
  // otherwise nothing on a row says whether it's a VM on the local hypervisor or one in vCenter.
  const providers = catalog?.providers ?? [];
  const showInfra =
    providers.length > 1 || new Set(clusters.map(clusterProvider)).size > 1;

  const donutData = useMemo(() => {
    const byPhase = new Map<string, number>();
    for (const c of clusters) byPhase.set(c.phase, (byPhase.get(c.phase) ?? 0) + 1);
    return [...byPhase.entries()].map(([phase, value]) => ({
      name: phaseMeta(phase).label,
      value,
      color: cssColor(phaseMeta(phase).color),
    }));
  }, [clusters]);

  const recent = useMemo(
    () => [...clusters].sort((a, b) => (a.created_at < b.created_at ? 1 : -1)).slice(0, 6),
    [clusters],
  );

  return (
    <>
      <Group justify="space-between" mb="lg">
        <div>
          <Title order={2}>Overview</Title>
          <Text c="dimmed" size="sm">
            Fleet health and host capacity at a glance.
          </Text>
        </div>
        <Button component={Link} to="/clusters/new" leftSection={<IconPlus size={18} />}>
          New cluster
        </Button>
      </Group>

      <SimpleGrid cols={{ base: 2, md: 4 }} mb="md">
        <StatCard icon={IconStack2} label="Clusters" value={clusters.length} color="brand" />
        <StatCard icon={IconCircleCheck} label="Ready" value={counts.ready} color="teal" />
        <StatCard icon={IconLoader2} label="Provisioning" value={counts.provisioning} color="violet" />
        <StatCard icon={IconAlertTriangle} label="Failed" value={counts.failed} color="red" />
      </SimpleGrid>

      <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md" mb="md">
        <Card radius="md" padding="lg">
          <Text fw={600} mb="md">
            Your quota
          </Text>
          {cap ? (
            cap.total_vcpu === 0 && cap.total_mem_mb === 0 ? (
              <Center h={140}>
                <Text c="dimmed" size="sm" ta="center">
                  You don't have any quota yet. Ask an administrator to grant you capacity before
                  creating clusters.
                </Text>
              </Center>
            ) : (
              <>
                <CapacityGauges cap={cap} />
                {(cap.providers?.length ?? 0) > 1 && (
                  <Text size="xs" c="dimmed" mt="sm">
                    Quota is granted per infrastructure and can't be moved between them - a cluster
                    is admitted against the headroom on the infrastructure it runs on.
                  </Text>
                )}
              </>
            )
          ) : (
            <Stack>
              <Skeleton height={64} radius="md" />
              <Skeleton height={64} radius="md" />
            </Stack>
          )}
        </Card>

        <Card radius="md" padding="lg">
          <Text fw={600} mb="md">
            Phase distribution
          </Text>
          {clusters.length === 0 ? (
            <Center h={180}>
              <Text c="dimmed" size="sm">
                No clusters to chart yet.
              </Text>
            </Center>
          ) : (
            <Center>
              <DonutChart
                data={donutData}
                size={180}
                thickness={26}
                withTooltip
                tooltipDataSource="segment"
                chartLabel={`${clusters.length} total`}
              />
            </Center>
          )}
        </Card>
      </SimpleGrid>

      <Card radius="md" padding="lg">
        <Group justify="space-between" mb="sm">
          <Text fw={600}>Recent clusters</Text>
          <Anchor component={Link} to="/clusters" size="sm">
            View all
          </Anchor>
        </Group>
        {isLoading ? (
          <Stack>
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} height={40} radius="sm" />
            ))}
          </Stack>
        ) : recent.length === 0 ? (
          <EmptyState
            icon={IconServer2}
            title="Nothing running"
            description="Create a cluster and watch it converge to Ready in real time."
            action={
              <Button component={Link} to="/clusters/new" variant="light" mt="sm" leftSection={<IconPlus size={16} />}>
                New cluster
              </Button>
            }
          />
        ) : (
          <Stack gap="xs">
            {recent.map((c) => (
              <UnstyledButton key={c.id} component={Link} to={`/clusters/${c.id}`} className="recent-row" p="xs">
                <Group justify="space-between">
                  <Group gap="sm">
                    <ClusterStatusBadge phase={c.phase} />
                    <Text fw={600} size="sm">
                      {c.name}
                    </Text>
                    <Badge variant="default" size="xs" radius="sm">
                      {c.size}
                    </Badge>
                    {showInfra && <ProviderBadge cluster={c} size="xs" />}
                  </Group>
                  <Text size="xs" c="dimmed">
                    k8s {c.k8s_version} · {relative(c.created_at)}
                  </Text>
                </Group>
              </UnstyledButton>
            ))}
          </Stack>
        )}
      </Card>
    </>
  );
}
