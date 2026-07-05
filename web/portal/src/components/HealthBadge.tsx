import { Badge } from '@mantine/core';
import { healthMeta } from '../lib/health';
import type { HealthStatus } from '../lib/types';

// The rolled-up cluster-health chip (Healthy / Degraded / Unhealthy / Unknown), shown next to the
// phase badge in the header and reused wherever a compact health indicator is wanted.
export function HealthBadge({ status, size = 'sm' }: { status: HealthStatus | string; size?: string }) {
  const meta = healthMeta(status);
  const Icon = meta.icon;
  return (
    <Badge color={meta.color} variant="light" size={size} radius="sm" leftSection={<Icon size={12} />}>
      {meta.label}
    </Badge>
  );
}
