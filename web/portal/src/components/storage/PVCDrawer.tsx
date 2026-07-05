// The per-claim detail drawer: opens when a row in the Claims table is clicked, and shows the claim
// three ways - an Overview (binding state, the bound PersistentVolume, the pods mounting it), its
// Events (where a provisioning failure explains itself), and its YAML.

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
  ScrollArea,
  Tooltip,
  Skeleton,
  Alert,
} from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import type { PVCSummary, PVCDetail } from '../../lib/types';
import type { PVCRef } from '../../lib/api';
import { ApiError } from '../../lib/api';
import { usePVC, usePVCEvents, usePVCManifest } from '../../lib/queries';
import { pvcStatusColor, accessModeLabel } from '../../lib/storage';
import { relative } from '../../lib/format';
import { YamlView } from '../YamlView';
import { LabelBadges, DetailRow, EventsTable } from './shared';

export function PVCDrawer({
  clusterId,
  pvc,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  pvc: PVCSummary | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<string | null>('overview');
  const ref: PVCRef = { namespace: pvc?.namespace ?? '', name: pvc?.name ?? '' };
  const active = opened && !!pvc;

  const { data: detail, isLoading, error } = usePVC(clusterId, ref, active);

  return (
    <Drawer
      opened={opened}
      onClose={onClose}
      position="right"
      size="xl"
      title={
        <Group gap="sm" wrap="nowrap">
          <Text fw={700} style={{ wordBreak: 'break-all' }}>
            {pvc?.name}
          </Text>
          {pvc && (
            <Badge color={pvcStatusColor(pvc.status)} variant="light" size="sm">
              {pvc.status}
            </Badge>
          )}
          <Text size="sm" c="dimmed">
            {pvc?.namespace}
          </Text>
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
          {error ? (
            <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Could not load claim">
              {error instanceof ApiError ? error.message : String(error)}
            </Alert>
          ) : isLoading && !detail ? (
            <Stack>
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} height={70} radius="md" />
              ))}
            </Stack>
          ) : detail ? (
            <PVCOverview detail={detail} />
          ) : null}
        </Tabs.Panel>

        <Tabs.Panel value="events">
          <PVCEvents clusterId={clusterId} pvcRef={ref} enabled={active && tab === 'events'} />
        </Tabs.Panel>

        <Tabs.Panel value="yaml">
          <PVCYaml clusterId={clusterId} pvcRef={ref} enabled={active && tab === 'yaml'} />
        </Tabs.Panel>
      </Tabs>
    </Drawer>
  );
}

// ---- Overview ----------------------------------------------------------------

function PVCOverview({ detail }: { detail: PVCDetail }) {
  const usedBy = detail.used_by ?? [];
  const modes = detail.access_modes ?? [];
  return (
    <Stack gap="lg">
      <SimpleGrid cols={{ base: 2, sm: 3 }}>
        <Stat label="Status" value={
          <Badge color={pvcStatusColor(detail.status)} variant="dot" size="lg">
            {detail.status}
          </Badge>
        } />
        <Stat label="Capacity" value={detail.capacity || '-'} />
        <Stat label="Requested" value={detail.requested || '-'} />
      </SimpleGrid>

      <Stack gap={6}>
        <DetailRow label="Storage class" value={detail.storage_class || '-'} mono={!!detail.storage_class} />
        <DetailRow label="Volume mode" value={detail.volume_mode || '-'} />
        <DetailRow
          label="Access modes"
          value={
            modes.length > 0 ? (
              <Group gap={6}>
                {modes.map((m) => (
                  <Tooltip key={m} label={accessModeLabel(m)} multiline w={260}>
                    <Badge variant="outline" color="gray" size="sm" radius="sm">
                      {m}
                    </Badge>
                  </Tooltip>
                ))}
              </Group>
            ) : (
              '-'
            )
          }
        />
        <DetailRow label="Created" value={relative(detail.created_at)} />
      </Stack>

      {/* The bound PV. A Pending claim has none - say so plainly rather than showing an empty card. */}
      <section>
        <Text fw={600} mb="xs">
          Persistent volume
        </Text>
        {detail.persistent_volume ? (
          <Stack gap={6}>
            <DetailRow label="Name" value={detail.persistent_volume.name} mono />
            <DetailRow label="Capacity" value={detail.persistent_volume.capacity || '-'} />
            <DetailRow label="Status" value={detail.persistent_volume.status || '-'} />
            <DetailRow label="Reclaim policy" value={detail.persistent_volume.reclaim_policy || '-'} />
            <DetailRow
              label="Source"
              value={detail.persistent_volume.source || '-'}
              mono={!!detail.persistent_volume.source}
            />
          </Stack>
        ) : (
          <Text c="dimmed" size="sm">
            This claim is not bound to a volume yet - check its Events for why.
          </Text>
        )}
      </section>

      <section>
        <Text fw={600} mb="xs">
          Used by
        </Text>
        {usedBy.length > 0 ? (
          <Group gap="xs">
            {usedBy.map((p) => (
              // textTransform none: a pod name is a lowercase identifier, and Mantine's Badge
              // uppercases its content by default - which would render a name that doesn't exist.
              <Badge key={p} variant="light" color="blue" size="sm" radius="sm" style={{ textTransform: 'none' }}>
                {p}
              </Badge>
            ))}
          </Group>
        ) : (
          <Text c="dimmed" size="sm">
            No pods are currently mounting this claim.
          </Text>
        )}
      </section>

      <LabelBadges title="Labels" values={detail.labels} />
      <LabelBadges title="Annotations" values={detail.annotations} />

      {detail.conditions && detail.conditions.length > 0 && (
        <section>
          <Text fw={600} mb="xs">
            Conditions
          </Text>
          <Table withTableBorder>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Type</Table.Th>
                <Table.Th>Status</Table.Th>
                <Table.Th>Reason</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {detail.conditions.map((c) => (
                <Table.Tr key={c.type}>
                  <Table.Td>{c.type}</Table.Td>
                  <Table.Td>
                    <Badge color={c.status === 'True' ? 'green' : 'gray'} variant="light" size="sm">
                      {c.status}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{c.reason || '-'}</Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </section>
      )}
    </Stack>
  );
}

function Stat({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
        {label}
      </Text>
      <Text size="lg" fw={600} component="div">
        {value}
      </Text>
    </div>
  );
}

// ---- Events / YAML -----------------------------------------------------------

function PVCEvents({
  clusterId,
  pvcRef,
  enabled,
}: {
  clusterId: string | undefined;
  pvcRef: PVCRef;
  enabled: boolean;
}) {
  const { data: events, isLoading, error } = usePVCEvents(clusterId, pvcRef, enabled);
  if (error) {
    return (
      <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Could not load events">
        {error instanceof ApiError ? error.message : String(error)}
      </Alert>
    );
  }
  if (isLoading && !events) return <Skeleton height={200} radius="md" />;
  return (
    <ScrollArea.Autosize mah={600}>
      <EventsTable events={events ?? []} empty="No recent events for this claim." />
    </ScrollArea.Autosize>
  );
}

function PVCYaml({
  clusterId,
  pvcRef,
  enabled,
}: {
  clusterId: string | undefined;
  pvcRef: PVCRef;
  enabled: boolean;
}) {
  const { data, isLoading, error } = usePVCManifest(clusterId, pvcRef, enabled);
  return <YamlView yaml={data} filename={pvcRef.name} isLoading={isLoading} error={error} />;
}
