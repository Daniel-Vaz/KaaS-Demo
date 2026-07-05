// One shared mapping from a HealthStatus to its colour, label, and icon - used by the health badge,
// the health panel, and the per-node health cells so the whole portal reads health the same way.
// Mirrors internal/domain.HealthStatus.

import {
  IconCircleCheck,
  IconAlertTriangle,
  IconAlertOctagon,
  IconCircleDashed,
  type Icon,
} from '@tabler/icons-react';
import type { HealthStatus } from './types';

export interface HealthMeta {
  label: string;
  color: string; // a Mantine colour name
  icon: Icon;
}

export const HEALTH_META: Record<HealthStatus, HealthMeta> = {
  healthy: { label: 'Healthy', color: 'teal', icon: IconCircleCheck },
  degraded: { label: 'Degraded', color: 'yellow', icon: IconAlertTriangle },
  unhealthy: { label: 'Unhealthy', color: 'red', icon: IconAlertOctagon },
  unknown: { label: 'Unknown', color: 'gray', icon: IconCircleDashed },
};

export function healthMeta(status: HealthStatus | string): HealthMeta {
  return HEALTH_META[status as HealthStatus] ?? HEALTH_META.unknown;
}
