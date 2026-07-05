// The per-Gateway detail drawer: Details and YAML. A Gateway's list row already carries everything
// the detail shows (addresses, listeners, per-listener status), so like the StorageClass drawer there
// is no second fetch for the details - only the YAML is loaded on open.

import { useState } from 'react';
import { Drawer, Group, Text, Badge, Stack, Tabs, Table, Tooltip } from '@mantine/core';
import type { GatewaySummary, ObjectRef } from '../../lib/types';
import { useGatewayManifest } from '../../lib/queries';
import { readyColor, gatewayStatusText } from '../../lib/network';
import { relative } from '../../lib/format';
import { YamlView } from '../YamlView';
import { LabelBadges, DetailRow } from '../storage/shared';

export function GatewayDrawer({
  clusterId,
  gateway,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  gateway: GatewaySummary | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<string | null>('details');
  const ref: ObjectRef = { namespace: gateway?.namespace ?? '', name: gateway?.name ?? '' };

  const { data: yaml, isLoading, error } = useGatewayManifest(
    clusterId,
    ref,
    opened && !!gateway && tab === 'yaml',
  );

  return (
    <Drawer
      opened={opened}
      onClose={onClose}
      position="right"
      size="xl"
      title={
        <Group gap="sm" wrap="nowrap">
          <Text fw={700} style={{ wordBreak: 'break-all' }}>
            {gateway?.name}
          </Text>
          {gateway?.is_default && (
            <Tooltip label="The gateway this platform creates for every cluster" withArrow>
              <Badge color="blue" variant="light" size="sm">
                default
              </Badge>
            </Tooltip>
          )}
        </Group>
      }
    >
      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="details">Details</Tabs.Tab>
          <Tabs.Tab value="yaml">YAML</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="details">{gateway && <GatewayDetails g={gateway} />}</Tabs.Panel>

        <Tabs.Panel value="yaml">
          <YamlView
            yaml={yaml}
            filename={gateway?.name ?? 'gateway'}
            isLoading={isLoading}
            error={error}
          />
        </Tabs.Panel>
      </Tabs>
    </Drawer>
  );
}

function GatewayDetails({ g }: { g: GatewaySummary }) {
  return (
    <Stack gap="lg">
      <Stack gap={6}>
        <DetailRow label="Namespace" value={g.namespace} mono />
        <DetailRow label="Gateway class" value={g.class} mono />
        <DetailRow label="Addresses" value={(g.addresses ?? []).join(', ') || '-'} mono />
        <DetailRow
          label="Status"
          value={
            <Badge color={readyColor(g.programmed)} variant="light" size="sm">
              {g.programmed ? 'Programmed' : 'Not programmed'}
            </Badge>
          }
        />
        {!g.programmed && g.status && <DetailRow label="Reason" value={g.status} />}
        <DetailRow label="Created" value={relative(g.created_at)} />
      </Stack>

      {/* Listeners are where the interesting configuration lives: which ports are open, which
          hostnames they serve, and whether TLS terminates - the platform's HTTPS listener carries the
          wildcard certificate cert-manager issues for the cluster's apps domain. */}
      <section>
        <Text fw={600} mb="xs">
          Listeners
        </Text>
        <Table verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Name</Table.Th>
              <Table.Th>Protocol</Table.Th>
              <Table.Th>Port</Table.Th>
              <Table.Th>Hostname</Table.Th>
              <Table.Th>TLS</Table.Th>
              <Table.Th>Routes</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(g.listeners ?? []).map((l) => (
              <Table.Tr key={l.name}>
                <Table.Td>{l.name}</Table.Td>
                <Table.Td>{l.protocol}</Table.Td>
                <Table.Td ff="monospace">{l.port}</Table.Td>
                <Table.Td ff="monospace" style={{ wordBreak: 'break-all' }}>
                  {l.hostname || '*'}
                </Table.Td>
                <Table.Td>
                  {l.tls_mode ? (
                    <Tooltip
                      label={`${l.tls_mode} - ${(l.certificate_refs ?? []).join(', ') || 'no certificate ref'}`}
                      withArrow
                    >
                      <Badge color="green" variant="light" size="sm">
                        {l.tls_mode}
                      </Badge>
                    </Tooltip>
                  ) : (
                    <Text size="sm" c="dimmed">
                      -
                    </Text>
                  )}
                </Table.Td>
                <Table.Td>{l.attached_routes}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        <Text c="dimmed" size="xs" mt="xs">
          {gatewayStatusText(g)}
        </Text>
      </section>

      <LabelBadges title="Labels" values={g.labels} />
    </Stack>
  );
}
