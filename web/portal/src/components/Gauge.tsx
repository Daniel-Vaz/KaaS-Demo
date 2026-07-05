import { Group, Progress, Paper, Text } from '@mantine/core';
import type { Icon } from '@tabler/icons-react';
import { pct } from '../lib/format';

// A single labelled usage gauge - CPU and memory always render as a same-size pair (never one
// bolded as a headline and the other demoted to a footnote), so the two are equally scannable at
// a glance. Shared by CapacityGauges, ClusterUsageCard, and the admin allocation cards.
export function Gauge({
  icon: IconCmp,
  label,
  used,
  total,
  display,
  withBorder = false,
}: {
  icon: Icon;
  label: string;
  used: number;
  total: number;
  display: string;
  withBorder?: boolean;
}) {
  const value = pct(used, total);
  const hot = value >= 90;
  const color = hot ? 'red' : value >= 70 ? 'yellow' : 'brand';
  return (
    <Paper p="md" radius="md" withBorder={withBorder}>
      <Group justify="space-between" mb={6}>
        <Group gap={6}>
          <IconCmp size={16} stroke={1.6} />
          <Text size="sm" fw={600}>
            {label}
          </Text>
        </Group>
        <Text size="sm" c="dimmed">
          {display} · {value}%
        </Text>
      </Group>
      <Progress value={value} color={color} size="lg" radius="xl" striped={hot} animated={hot} />
    </Paper>
  );
}
