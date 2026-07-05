import { Card, Group, SimpleGrid, Text, Skeleton } from '@mantine/core';
import { IconCpu, IconDatabase, IconChartBar } from '@tabler/icons-react';
import { cores, gibBytes, relative } from '../lib/format';
import { Gauge } from './Gauge';
import type { MetricsSnapshot } from '../lib/types';

// ClusterUsageCard shows cluster-wide CPU/memory consumption, summed across the per-node samples
// in the latest metrics snapshot. Rendered only when metrics-server is enabled; while the first
// snapshot is still pending it shows a gathering-state skeleton.
export function ClusterUsageCard({ snapshot }: { snapshot: MetricsSnapshot | null | undefined }) {
  const nodes = snapshot?.nodes ?? [];
  const totals = nodes.reduce(
    (acc, n) => ({
      cpuUsed: acc.cpuUsed + n.cpu_used_milli,
      cpuCap: acc.cpuCap + n.cpu_capacity_milli,
      memUsed: acc.memUsed + n.mem_used_bytes,
      memCap: acc.memCap + n.mem_capacity_bytes,
    }),
    { cpuUsed: 0, cpuCap: 0, memUsed: 0, memCap: 0 },
  );

  return (
    <Card radius="md" padding="lg" mb="md">
      <Group justify="space-between" mb="md">
        <Group gap={8}>
          <IconChartBar size={18} />
          <Text fw={600}>Resource usage</Text>
        </Group>
        {snapshot ? (
          <Text size="xs" c="dimmed">
            updated {relative(snapshot.collected_at)}
          </Text>
        ) : (
          <Text size="xs" c="dimmed">
            gathering metrics…
          </Text>
        )}
      </Group>
      {snapshot && nodes.length > 0 ? (
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md">
          <Gauge
            icon={IconCpu}
            label="CPU"
            used={totals.cpuUsed}
            total={totals.cpuCap}
            display={`${cores(totals.cpuUsed)} / ${cores(totals.cpuCap)} cores`}
            withBorder
          />
          <Gauge
            icon={IconDatabase}
            label="Memory"
            used={totals.memUsed}
            total={totals.memCap}
            display={`${gibBytes(totals.memUsed)} / ${gibBytes(totals.memCap)} GiB`}
            withBorder
          />
        </SimpleGrid>
      ) : (
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md">
          <Skeleton height={72} radius="md" />
          <Skeleton height={72} radius="md" />
        </SimpleGrid>
      )}
    </Card>
  );
}
