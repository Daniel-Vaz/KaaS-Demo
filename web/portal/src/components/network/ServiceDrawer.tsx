// The per-Service detail drawer: opens when a row in the Services table is clicked, and shows the
// Service three ways - an Overview (type, addresses, ports, and the endpoints actually behind it),
// its Events (where a LoadBalancer with no address explains itself), and its YAML.

import { useState } from 'react';
import {
  Drawer,
  Group,
  Text,
  Badge,
  Stack,
  Tabs,
  Table,
  SimpleGrid,
  Tooltip,
  Skeleton,
  Alert,
} from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import type { ServiceSummary, ObjectRef } from '../../lib/types';
import { ApiError } from '../../lib/api';
import { useService, useServiceEvents, useServiceManifest } from '../../lib/queries';
import { serviceTypeColor, serviceTypeLabel, formatPort } from '../../lib/network';
import { relative } from '../../lib/format';
import { YamlView } from '../YamlView';
import { LabelBadges, DetailRow, EventsTable } from '../storage/shared';

export function ServiceDrawer({
  clusterId,
  service,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  service: ServiceSummary | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<string | null>('overview');
  const ref: ObjectRef = { namespace: service?.namespace ?? '', name: service?.name ?? '' };
  const active = opened && !!service;

  const { data: detail, isLoading, error } = useService(clusterId, ref, active);
  const { data: events } = useServiceEvents(clusterId, ref, active && tab === 'events');
  const {
    data: yaml,
    isLoading: yamlLoading,
    error: yamlError,
  } = useServiceManifest(clusterId, ref, active && tab === 'yaml');

  return (
    <Drawer
      opened={opened}
      onClose={onClose}
      position="right"
      size="xl"
      title={
        <Group gap="sm" wrap="nowrap">
          <Text fw={700} style={{ wordBreak: 'break-all' }}>
            {service?.name}
          </Text>
          {service && (
            <Tooltip label={serviceTypeLabel(service.type)} withArrow>
              <Badge color={serviceTypeColor(service.type)} variant="light" size="sm">
                {service.type}
              </Badge>
            </Tooltip>
          )}
        </Group>
      }
    >
      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="overview">Overview</Tabs.Tab>
          <Tabs.Tab value="events">Events</Tabs.Tab>
          <Tabs.Tab value="yaml">YAML</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="overview">
          {isLoading && !detail ? (
            <Stack gap="sm">
              <Skeleton height={20} />
              <Skeleton height={20} />
              <Skeleton height={120} />
            </Stack>
          ) : error ? (
            <Alert color="red" icon={<IconAlertTriangle size={18} />}>
              {error instanceof ApiError ? error.message : 'Could not load this service.'}
            </Alert>
          ) : detail ? (
            <Stack gap="lg">
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="xl">
                <Stack gap={6}>
                  <DetailRow label="Namespace" value={detail.namespace} mono />
                  <DetailRow label="Type" value={detail.type} />
                  <DetailRow label="Cluster IP" value={detail.cluster_ip || 'None (headless)'} mono />
                  <DetailRow label="Created" value={relative(detail.created_at)} />
                </Stack>
                <Stack gap={6}>
                  <DetailRow
                    label="External address"
                    value={(detail.external_ips ?? []).join(', ') || '-'}
                    mono
                  />
                  <DetailRow label="Session affinity" value={detail.session_affinity || '-'} />
                  <DetailRow
                    label="External traffic policy"
                    value={detail.external_traffic_policy || '-'}
                  />
                  {detail.external_name && (
                    <DetailRow label="External name" value={detail.external_name} mono />
                  )}
                </Stack>
              </SimpleGrid>

              <section>
                <Text fw={600} mb="xs">
                  Ports
                </Text>
                <Table verticalSpacing="xs">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Name</Table.Th>
                      <Table.Th>Port</Table.Th>
                      <Table.Th>Target</Table.Th>
                      <Table.Th>Node port</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {(detail.ports ?? []).map((p, i) => (
                      <Table.Tr key={i}>
                        <Table.Td>{p.name || '-'}</Table.Td>
                        <Table.Td ff="monospace">{formatPort(p)}</Table.Td>
                        <Table.Td ff="monospace">{p.target_port || '-'}</Table.Td>
                        <Table.Td ff="monospace">{p.node_port || '-'}</Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </section>

              {/* The endpoints behind the Service: the "why is this 503" answer. An empty list on a
                  Service with a selector means nothing matched it - worth saying outright. */}
              <section>
                <Text fw={600} mb="xs">
                  Endpoints ({(detail.backends ?? []).length})
                </Text>
                {(detail.backends ?? []).length === 0 ? (
                  <Text c="dimmed" size="sm">
                    No ready endpoints - nothing is currently backing this service.
                  </Text>
                ) : (
                  <Table verticalSpacing="xs">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>Pod</Table.Th>
                        <Table.Th>IP</Table.Th>
                        <Table.Th>Node</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {(detail.backends ?? []).map((b) => (
                        <Table.Tr key={b.ip}>
                          <Table.Td style={{ wordBreak: 'break-all' }}>{b.pod || '-'}</Table.Td>
                          <Table.Td ff="monospace">{b.ip}</Table.Td>
                          <Table.Td>{b.node || '-'}</Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                )}
              </section>

              <LabelBadges title="Selector" values={detail.selector} />
              <LabelBadges title="Labels" values={detail.labels} />
              <LabelBadges title="Annotations" values={detail.annotations} />
            </Stack>
          ) : null}
        </Tabs.Panel>

        <Tabs.Panel value="events">
          <EventsTable events={events ?? []} empty="No events for this service." />
        </Tabs.Panel>

        <Tabs.Panel value="yaml">
          <YamlView
            yaml={yaml}
            filename={service?.name ?? 'service'}
            isLoading={yamlLoading}
            error={yamlError}
          />
        </Tabs.Panel>
      </Tabs>
    </Drawer>
  );
}
