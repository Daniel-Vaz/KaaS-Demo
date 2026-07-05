import { Card, Group, Text, Progress, ThemeIcon, Loader, Badge } from '@mantine/core';
import { IconClockHour4 } from '@tabler/icons-react';
import { duration, secondsSince } from '../lib/format';
import { workerCount } from '../lib/cluster';
import { nodeColor } from '../lib/phase';
import type { Cluster, Operation } from '../lib/types';

// WorkerScaleProgress shows the live convergence of the cluster's workers toward their desired
// total, mirroring the upgrade progress view. Rendered only while the provisioned worker count
// differs from the total desired across all node pools - the level-triggered signal that the
// reconciler is still converging. It deliberately reports the fleet-wide total rather than a
// per-pool breakdown: adding a pool, removing one and scaling one are all just "workers are moving",
// and the per-pool detail lives on the Node pools panel. `op` is the in-progress scale operation
// (for the elapsed clock).
export function WorkerScaleProgress({ cluster, op }: { cluster: Cluster; op?: Operation }) {
  const workers = (cluster.nodes ?? []).filter((n) => n.role === 'worker');
  const current = workers.length;
  const target = workerCount(cluster);
  // Only a genuine scale - not initial bring-up (which also starts with 0 of N workers). A live
  // scale is signalled either by an in-progress scale operation or the Updating phase.
  const scaling = current !== target && (!!op || cluster.phase === 'Updating');
  if (!scaling) return null;

  const scalingDown = target < current;
  // Rough convergence ratio: the two counts approach each other as the reconciler adds/removes VMs.
  const pctValue = Math.round((Math.min(current, target) / Math.max(current, target, 1)) * 100);
  const elapsed = op ? secondsSince(op.started_at) : 0;
  const delta = Math.abs(target - current);

  return (
    <Card radius="md" padding="lg" withBorder>
      <Group justify="space-between" mb="xs" wrap="nowrap">
        <Group gap="xs">
          <ThemeIcon variant="light" color="blue" radius="xl" size={28}>
            <Loader size={14} color="blue" />
          </ThemeIcon>
          <Text fw={600}>
            {scalingDown ? 'Scaling down' : 'Scaling up'} workers {current} → {target}
          </Text>
        </Group>
        <Group gap="xs" c="dimmed">
          <IconClockHour4 size={14} />
          <Text size="xs">{op ? `elapsed ${duration(elapsed)}` : 'in progress'}</Text>
        </Group>
      </Group>

      <Progress value={pctValue} color="blue" radius="xl" size="sm" mb="md" striped animated />

      <Text size="sm" c="dimmed" mb={workers.length ? 6 : 0}>
        {scalingDown
          ? `Draining and removing ${delta} worker(s) - the reconciler cordons each node before its VM is destroyed.`
          : `Provisioning and joining ${delta} worker(s) - new VMs are created, then kubeadm-joined.`}
      </Text>
      {workers.length > 0 && (
        <Group gap="xs">
          {workers.map((n) => (
            <Badge key={n.vm_name} size="sm" variant="dot" color={nodeColor(n.phase)}>
              {n.vm_name}
            </Badge>
          ))}
        </Group>
      )}
      <Text size="xs" c="dimmed" mt="md">
        Follow every step live in the <b>Activity</b> tab.
      </Text>
    </Card>
  );
}
