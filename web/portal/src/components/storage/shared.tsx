// Small presentational pieces shared by the Storage page's two drawers (claims and classes): a
// label/value row, a key=value badge block, and the events table.

import type { ReactNode } from 'react';
import { Group, Text, Badge, Table } from '@mantine/core';
import type { WorkloadEvent } from '../../lib/types';
import { relative } from '../../lib/format';

// DetailRow is one "label - value" line in a drawer's detail list. mono renders the value in the
// monospace face, for values that are identifiers (a PV name, a provisioner) rather than prose.
export function DetailRow({
  label,
  value,
  mono,
}: {
  label: string;
  value: ReactNode;
  mono?: boolean;
}) {
  return (
    <Group justify="space-between" wrap="nowrap" align="flex-start" gap="md">
      <Text size="sm" c="dimmed" style={{ flex: '0 0 auto' }}>
        {label}
      </Text>
      {typeof value === 'string' ? (
        <Text size="sm" ff={mono ? 'monospace' : undefined} ta="right" style={{ wordBreak: 'break-all' }}>
          {value}
        </Text>
      ) : (
        <div>{value}</div>
      )}
    </Group>
  );
}

// LabelBadges renders a key=value map as badges (labels, annotations, StorageClass parameters).
// Renders nothing at all when the map is empty - an empty section is noise.
export function LabelBadges({
  title,
  values,
}: {
  title: string;
  values?: Record<string, string>;
}) {
  const entries = Object.entries(values ?? {});
  if (entries.length === 0) return null;
  return (
    <section>
      <Text fw={600} mb="xs">
        {title}
      </Text>
      <Group gap="xs">
        {entries.map(([k, v]) => (
          <Badge key={k} variant="outline" color="gray" size="sm" radius="sm" style={{ textTransform: 'none' }}>
            {v === '' ? k : `${k}=${v}`}
          </Badge>
        ))}
      </Group>
    </section>
  );
}

// EventsTable renders Kubernetes events newest-first. Warnings are colour-coded and also labelled,
// so severity never rests on colour alone.
export function EventsTable({ events, empty }: { events: WorkloadEvent[]; empty: string }) {
  if (events.length === 0) {
    return (
      <Text c="dimmed" size="sm" py="md">
        {empty}
      </Text>
    );
  }
  return (
    <Table verticalSpacing="sm">
      <Table.Thead>
        <Table.Tr>
          <Table.Th>Type</Table.Th>
          <Table.Th>Reason</Table.Th>
          <Table.Th>Message</Table.Th>
          <Table.Th>Count</Table.Th>
          <Table.Th>Last seen</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {events.map((e, i) => (
          <Table.Tr key={i}>
            <Table.Td>
              <Badge color={e.type === 'Warning' ? 'red' : 'gray'} variant="light" size="sm">
                {e.type}
              </Badge>
            </Table.Td>
            <Table.Td>
              <Text size="sm">{e.reason}</Text>
            </Table.Td>
            <Table.Td>
              <Text size="sm">{e.message}</Text>
            </Table.Td>
            <Table.Td>{e.count}</Table.Td>
            <Table.Td>
              <Text size="sm" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
                {relative(e.last_seen)}
              </Text>
            </Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}
