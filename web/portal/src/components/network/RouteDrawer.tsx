// The per-route detail drawer: Details and YAML. Like the Gateway drawer, the list row already
// carries the whole detail (hostnames, parents, rules), so only the YAML is fetched on open.

import { useState } from 'react';
import { Drawer, Group, Text, Badge, Stack, Tabs, Table, Code } from '@mantine/core';
import type { RouteSummary, ObjectRef } from '../../lib/types';
import { useRouteManifest } from '../../lib/queries';
import { readyColor, routeKindLabel } from '../../lib/network';
import { relative } from '../../lib/format';
import { YamlView } from '../YamlView';
import { LabelBadges, DetailRow } from '../storage/shared';

export function RouteDrawer({
  clusterId,
  route,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  route: RouteSummary | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<string | null>('details');
  const ref: ObjectRef = { namespace: route?.namespace ?? '', name: route?.name ?? '' };

  const { data: yaml, isLoading, error } = useRouteManifest(
    clusterId,
    route?.kind ?? 'httproute',
    ref,
    opened && !!route && tab === 'yaml',
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
            {route?.name}
          </Text>
          {route && (
            <Badge color="gray" variant="light" size="sm">
              {routeKindLabel(route.kind)}
            </Badge>
          )}
        </Group>
      }
    >
      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="details">Details</Tabs.Tab>
          <Tabs.Tab value="yaml">YAML</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="details">{route && <RouteDetails r={route} />}</Tabs.Panel>

        <Tabs.Panel value="yaml">
          <YamlView
            yaml={yaml}
            filename={route?.name ?? 'route'}
            isLoading={isLoading}
            error={error}
          />
        </Tabs.Panel>
      </Tabs>
    </Drawer>
  );
}

function RouteDetails({ r }: { r: RouteSummary }) {
  return (
    <Stack gap="lg">
      <Stack gap={6}>
        <DetailRow label="Namespace" value={r.namespace} mono />
        <DetailRow label="Kind" value={routeKindLabel(r.kind)} />
        <DetailRow
          label="Status"
          value={
            <Badge color={readyColor(r.accepted)} variant="light" size="sm">
              {r.accepted ? 'Accepted' : 'Not accepted'}
            </Badge>
          }
        />
        {!r.accepted && r.status && <DetailRow label="Reason" value={r.status} />}
        <DetailRow label="Created" value={relative(r.created_at)} />
      </Stack>

      <section>
        <Text fw={600} mb="xs">
          Hostnames
        </Text>
        {(r.hostnames ?? []).length === 0 ? (
          <Text c="dimmed" size="sm">
            None - this route matches every hostname its listener serves.
          </Text>
        ) : (
          <Group gap="xs">
            {(r.hostnames ?? []).map((h) => (
              <Code key={h}>{h}</Code>
            ))}
          </Group>
        )}
      </section>

      {/* The parents are the Gateways this route attaches to. Acceptance is per-parent, and a
          refused one is where "my route does nothing" is explained. */}
      <section>
        <Text fw={600} mb="xs">
          Attached to
        </Text>
        <Table verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Gateway</Table.Th>
              <Table.Th>Listener</Table.Th>
              <Table.Th>Accepted</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(r.parent_refs ?? []).map((p, i) => (
              <Table.Tr key={i}>
                <Table.Td ff="monospace">
                  {p.namespace}/{p.name}
                </Table.Td>
                <Table.Td>{p.section_name || 'any'}</Table.Td>
                <Table.Td>
                  <Badge color={readyColor(p.accepted)} variant="light" size="sm">
                    {p.accepted ? 'Yes' : p.status || 'Pending'}
                  </Badge>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </section>

      <section>
        <Text fw={600} mb="xs">
          Rules
        </Text>
        <Table verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Matches</Table.Th>
              <Table.Th>Backends</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(r.rules ?? []).map((rule, i) => (
              <Table.Tr key={i}>
                <Table.Td>
                  {(rule.matches ?? []).length === 0 ? (
                    <Text size="sm" c="dimmed">
                      everything
                    </Text>
                  ) : (
                    <Group gap="xs">
                      {(rule.matches ?? []).map((m) => (
                        <Code key={m}>{m}</Code>
                      ))}
                    </Group>
                  )}
                </Table.Td>
                <Table.Td>
                  <Group gap="xs">
                    {(rule.backends ?? []).map((b, j) => (
                      <Code key={j}>
                        {b.name}
                        {b.port ? `:${b.port}` : ''}
                      </Code>
                    ))}
                  </Group>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </section>

      <LabelBadges title="Labels" values={r.labels} />
    </Stack>
  );
}
