import { Table, Badge, Text, Group, Code, Progress, Tooltip, ActionIcon } from '@mantine/core';
import { IconServer, IconServerBolt, IconDeviceSdCard, IconTerminal2 } from '@tabler/icons-react';
import { nodeColor } from '../lib/phase';
import { cores, gibBytes, pct } from '../lib/format';
import type { Node, NodeMetrics, NodeHealth, NodeDisk } from '../lib/types';
import classes from './NodeTable.module.css';

// A per-node health cell: a Ready/NotReady dot, a Cordoned warning chip (spec.unschedulable -
// `kubectl cordon`/drain - which doesn't affect Ready, so it needs its own signal), and a chip for
// any active node pressure condition (MemoryPressure/DiskPressure/PIDPressure).
function HealthCell({ h }: { h?: NodeHealth }) {
  if (!h) return <Text c="dimmed">-</Text>;
  return (
    <Group gap={6} wrap="nowrap">
      <Badge size="sm" variant="dot" color={h.ready ? 'teal' : 'red'}>
        {h.ready ? 'Ready' : 'NotReady'}
      </Badge>
      {h.cordoned && (
        <Tooltip label="Cordoned - unschedulable for new pods" withArrow>
          <Badge size="xs" variant="light" color="orange">
            Cordoned
          </Badge>
        </Tooltip>
      )}
      {(h.pressures ?? []).map((p) => (
        <Tooltip key={p} label={p} withArrow>
          <Badge size="xs" variant="light" color="yellow">
            {p.replace('Pressure', '')}
          </Badge>
        </Tooltip>
      ))}
    </Group>
  );
}

// A compact usage cell: a small bar plus the percentage, tooltip'd with used/total. Reuses the
// same red/yellow/brand thresholds as the cluster-wide gauges so the two read consistently.
function UsageCell({ used, total, detail }: { used: number; total: number; detail: string }) {
  const value = pct(used, total);
  const color = value >= 90 ? 'red' : value >= 70 ? 'yellow' : 'brand';
  return (
    <Tooltip label={detail} withArrow>
      <Group gap={8} wrap="nowrap" miw={110}>
        <Progress value={value} color={color} size="sm" radius="xl" style={{ flex: 1 }} />
        <Text size="xs" c="dimmed" w={32} ta="right">
          {value}%
        </Text>
      </Group>
    </Tooltip>
  );
}

export function NodeTable({
  nodes,
  metrics,
  health,
  disks,
  onSelect,
  onSsh,
}: {
  nodes: Node[] | null;
  metrics?: NodeMetrics[];
  health?: NodeHealth[];
  // The cluster's extra disks. Only a per-node COUNT is shown here - the table has to stay scannable
  // across a whole cluster, so the disks themselves live in the node's detail pane.
  disks?: NodeDisk[];
  // Selecting a row opens that pane. Absent (e.g. a read-only embed) leaves rows inert.
  onSelect?: (n: Node) => void;
  // Opens an SSH session to a node. Present only for write-access actors (the API is the authoritative
  // gate), so its presence is what adds the trailing actions column - same conditional-column idiom as
  // usage/health/disks. A node with no IP yet is disabled with a reason.
  onSsh?: (n: Node) => void;
}) {
  const list = nodes ?? [];
  if (list.length === 0) {
    return (
      <Text c="dimmed" size="sm" py="sm">
        No nodes provisioned yet.
      </Text>
    );
  }
  const byNode = new Map((metrics ?? []).map((m) => [m.node_name, m] as const));
  const healthByNode = new Map((health ?? []).map((h) => [h.node_name, h] as const));
  const showUsage = (metrics?.length ?? 0) > 0;
  const showHealth = (health?.length ?? 0) > 0;
  // Same conditional-column idiom as usage/health: a cluster with no extra disks anywhere shouldn't
  // carry a column of dashes.
  const diskList = disks ?? [];
  const showDisks = diskList.length > 0;
  const showSsh = !!onSsh;
  const disksByNode = new Map<string, NodeDisk[]>();
  for (const d of diskList) {
    disksByNode.set(d.vm_name, [...(disksByNode.get(d.vm_name) ?? []), d]);
  }
  return (
    <Table.ScrollContainer
      minWidth={
        620 + (showHealth ? 180 : 0) + (showUsage ? 200 : 0) + (showDisks ? 90 : 0) + (showSsh ? 60 : 0)
      }
    >
      <Table verticalSpacing="xs" highlightOnHover>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>VM</Table.Th>
            <Table.Th>Role</Table.Th>
            <Table.Th>Pool</Table.Th>
            <Table.Th>IP</Table.Th>
            <Table.Th>Phase</Table.Th>
            {showDisks && <Table.Th>Disks</Table.Th>}
            {showHealth && <Table.Th>Health</Table.Th>}
            {showUsage && <Table.Th>CPU</Table.Th>}
            {showUsage && <Table.Th>Memory</Table.Th>}
            {showSsh && <Table.Th aria-label="Actions" />}
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {list.map((n) => {
            const isCP = n.role === 'control-plane';
            const m = byNode.get(n.vm_name);
            const nodeDisks = disksByNode.get(n.vm_name) ?? [];
            return (
              <Table.Tr
                key={n.id || n.vm_name}
                className={classes.row}
                onClick={onSelect ? () => onSelect(n) : undefined}
                style={onSelect ? { cursor: 'pointer' } : undefined}
              >
                <Table.Td>
                  <Group gap={6} wrap="nowrap">
                    {isCP ? <IconServerBolt size={15} /> : <IconServer size={15} />}
                    <Text size="sm" ff="monospace">
                      {n.vm_name}
                    </Text>
                  </Group>
                </Table.Td>
                <Table.Td>
                  <Badge size="xs" variant="outline" color={isCP ? 'grape' : 'gray'} radius="sm">
                    {n.role}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  {/* Control planes belong to no pool - pools are workers only. */}
                  {n.pool ? (
                    <Badge size="xs" variant="light" color="brand" radius="sm">
                      {n.pool}
                    </Badge>
                  ) : (
                    <Text c="dimmed">-</Text>
                  )}
                </Table.Td>
                <Table.Td>{n.ip ? <Code>{n.ip}</Code> : <Text c="dimmed">-</Text>}</Table.Td>
                <Table.Td>
                  <Badge size="sm" variant="dot" color={nodeColor(n.phase)}>
                    {n.phase || 'pending'}
                  </Badge>
                </Table.Td>
                {showDisks && (
                  <Table.Td>
                    {nodeDisks.length > 0 ? (
                      <Tooltip
                        label={nodeDisks
                          .map((d) => `${d.name} - ${d.size_gb} GB at ${d.mount_path}`)
                          .join('\n')}
                        multiline
                        withArrow
                      >
                        <Badge
                          size="xs"
                          variant="light"
                          color="cyan"
                          radius="sm"
                          leftSection={<IconDeviceSdCard size={11} />}
                        >
                          {nodeDisks.length}
                        </Badge>
                      </Tooltip>
                    ) : (
                      <Text c="dimmed">-</Text>
                    )}
                  </Table.Td>
                )}
                {showHealth && (
                  <Table.Td>
                    <HealthCell h={healthByNode.get(n.vm_name)} />
                  </Table.Td>
                )}
                {showUsage && (
                  <Table.Td>
                    {m ? (
                      <UsageCell
                        used={m.cpu_used_milli}
                        total={m.cpu_capacity_milli}
                        detail={`${cores(m.cpu_used_milli)} / ${cores(m.cpu_capacity_milli)} cores`}
                      />
                    ) : (
                      <Text c="dimmed">-</Text>
                    )}
                  </Table.Td>
                )}
                {showUsage && (
                  <Table.Td>
                    {m ? (
                      <UsageCell
                        used={m.mem_used_bytes}
                        total={m.mem_capacity_bytes}
                        detail={`${gibBytes(m.mem_used_bytes)} / ${gibBytes(m.mem_capacity_bytes)} GiB`}
                      />
                    ) : (
                      <Text c="dimmed">-</Text>
                    )}
                  </Table.Td>
                )}
                {showSsh && (
                  <Table.Td ta="right">
                    <Tooltip label={n.ip ? `SSH to ${n.vm_name}` : 'No IP yet - provisioning'} withArrow>
                      {/* stopPropagation: the whole row is a click target (opens the detail pane); the
                          SSH icon must not also trigger it. A span wraps the disabled icon so the
                          tooltip still fires (a disabled button emits no pointer events). */}
                      <span
                        className={classes.sshAction}
                        onClick={(e) => e.stopPropagation()}
                        style={{ display: 'inline-flex' }}
                      >
                        <ActionIcon
                          variant="subtle"
                          color="gray"
                          disabled={!n.ip}
                          aria-label={`SSH to ${n.vm_name}`}
                          onClick={() => onSsh?.(n)}
                        >
                          <IconTerminal2 size={16} />
                        </ActionIcon>
                      </span>
                    </Tooltip>
                  </Table.Td>
                )}
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}
