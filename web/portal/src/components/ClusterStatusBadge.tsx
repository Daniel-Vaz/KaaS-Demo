import { Badge, Loader, Group } from '@mantine/core';
import { phaseMeta } from '../lib/phase';
import type { Phase } from '../lib/types';

export function ClusterStatusBadge({ phase, size = 'sm' }: { phase: Phase | string; size?: string }) {
  const meta = phaseMeta(phase);
  return (
    <Badge
      color={meta.color}
      variant="light"
      size={size}
      radius="sm"
      leftSection={
        meta.active ? (
          <Group gap={0} align="center">
            <Loader size={10} color={meta.color} type="oval" />
          </Group>
        ) : undefined
      }
    >
      {meta.label}
    </Badge>
  );
}
