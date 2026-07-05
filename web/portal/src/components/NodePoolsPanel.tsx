import { useState } from 'react';
import { Stack, Group, Button, Text, Alert } from '@mantine/core';
import { IconInfoCircle, IconDeviceFloppy, IconEye, IconAlertTriangle } from '@tabler/icons-react';
import { useUpdateCluster } from '../lib/queries';
import { NodePoolEditor, poolsValid } from './NodePoolEditor';
import type { Cluster, NodePool } from '../lib/types';

// NodePoolsPanel manages a running cluster's worker topology: scale a pool, add one, remove one.
// All three are the same declarative edit - the whole desired pool list is sent and the server
// bumps the generation, after which the reconciler converges the live cluster (level-triggered).
// canManage gates the write: a read-only group member sees the pools but can't change them (the
// server enforces this too).
export function NodePoolsPanel({ cluster, canManage }: { cluster: Cluster; canManage: boolean }) {
  const update = useUpdateCluster(cluster.id);
  const current = cluster.node_pools ?? [];
  const [pools, setPools] = useState<NodePool[]>(current);

  if (!canManage) {
    return (
      <Alert variant="light" color="gray" icon={<IconEye size={16} />}>
        Read-only access - managing node pools is available to owners, group members with the{' '}
        <b>Write</b> role, and admins.
      </Alert>
    );
  }

  if (cluster.phase !== 'Ready') {
    return (
      <Alert variant="light" color="gray" icon={<IconInfoCircle size={16} />}>
        Node pool changes become available once the cluster is <b>Ready</b>.
      </Alert>
    );
  }

  const dirty = JSON.stringify(pools) !== JSON.stringify(current);
  const removed = current.filter((p) => !pools.some((q) => q.name === p.name));
  const valid = poolsValid(pools);

  return (
    <Stack>
      <Text size="sm" c="dimmed">
        Each pool is a group of worker nodes at one size, scaled independently. Nodes are named{' '}
        <code>{cluster.name}-&lt;pool&gt;-&lt;n&gt;</code> and carry a{' '}
        <code>kaas.io/nodepool</code> label, so workloads can target a pool with a nodeSelector.
      </Text>

      <NodePoolEditor
        pools={pools}
        onChange={setPools}
        locked={current.map((p) => p.name)}
        controlPlaneSize={cluster.size}
      />

      {removed.length > 0 && (
        <Alert variant="light" color="orange" icon={<IconAlertTriangle size={16} />}>
          Removing {removed.map((p) => `"${p.name}"`).join(', ')} will drain and destroy{' '}
          {removed.reduce((n, p) => n + p.desired_workers, 0)} worker(s). Pods on them are evicted and
          rescheduled onto the remaining pools.
        </Alert>
      )}

      <Group>
        <Button
          leftSection={<IconDeviceFloppy size={16} />}
          onClick={() => update.mutate({ node_pools: pools })}
          loading={update.isPending}
          disabled={!dirty || !valid}
        >
          Apply changes
        </Button>
        {dirty && valid && (
          <Text size="xs" c="dimmed">
            unsaved changes
          </Text>
        )}
      </Group>
    </Stack>
  );
}
