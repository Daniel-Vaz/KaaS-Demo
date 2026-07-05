import { Card, Group, Text, Stack, Paper, Skeleton, ThemeIcon } from '@mantine/core';
import { IconActivityHeartbeat } from '@tabler/icons-react';
import { healthMeta } from '../lib/health';
import { relative } from '../lib/format';
import { HealthBadge } from './HealthBadge';
import type { HealthSnapshot } from '../lib/types';

// HealthPanel is the Overview tab's health section: the rolled-up status plus a row per dedicated
// check (API server, nodes Ready, system workloads, scheduling capacity, etcd quorum, add-on
// availability), each with a coloured status icon and a one-line summary. It replaces the old thin
// Status card; the cluster's rolled-up status string is folded in as a footer line. While the first
// snapshot is still pending it shows an evaluating-state skeleton.
export function HealthPanel({
  snapshot,
  status,
}: {
  snapshot: HealthSnapshot | null | undefined;
  status?: string;
}) {
  return (
    <Card radius="md" padding="lg">
      <Group justify="space-between" mb="md">
        <Group gap={8}>
          <IconActivityHeartbeat size={18} />
          <Text fw={600}>Cluster health</Text>
          {snapshot && <HealthBadge status={snapshot.status} />}
        </Group>
        <Text size="xs" c="dimmed">
          {snapshot ? `evaluated ${relative(snapshot.collected_at)}` : 'evaluating…'}
        </Text>
      </Group>

      {snapshot ? (
        <Stack gap="xs">
          {snapshot.checks.map((c) => {
            const meta = healthMeta(c.status);
            const Icon = meta.icon;
            return (
              <Paper key={c.id} p="sm" radius="md" withBorder>
                <Group gap={10} wrap="nowrap" align="flex-start">
                  <ThemeIcon variant="light" color={meta.color} size="md" radius="xl">
                    <Icon size={16} />
                  </ThemeIcon>
                  <div style={{ minWidth: 0 }}>
                    <Text size="sm" fw={600}>
                      {c.name}
                    </Text>
                    <Text size="xs" c="dimmed">
                      {c.summary}
                    </Text>
                  </div>
                </Group>
              </Paper>
            );
          })}
          {status && (
            <Text size="xs" c="dimmed" mt={4}>
              {status}
            </Text>
          )}
        </Stack>
      ) : (
        <Stack gap="xs">
          <Skeleton height={52} radius="md" />
          <Skeleton height={52} radius="md" />
          <Skeleton height={52} radius="md" />
        </Stack>
      )}
    </Card>
  );
}
