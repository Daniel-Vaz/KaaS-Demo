import { useState, type ReactNode } from 'react';
import { Link, useParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import {
  Group,
  Title,
  Text,
  Badge,
  Button,
  Card,
  Tabs,
  Table,
  Stack,
  SimpleGrid,
  Breadcrumbs,
  Anchor,
  Modal,
  NumberInput,
  Skeleton,
  Alert,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { notifications } from '@mantine/notifications';
import {
  IconAlertTriangle,
  IconArrowsMaximize,
  IconChevronLeft,
} from '@tabler/icons-react';
import { useCluster, useWorkload, useWorkloadEvents, useScaleWorkload, qk } from '../lib/queries';
import { api, ApiError, type WorkloadRef } from '../lib/api';
import { useAuth } from '../lib/auth';
import { canManageCluster, clusterUsable } from '../lib/cluster';
import { relative } from '../lib/format';
import { kindLabel, kindColor, statusColor, scalable } from '../lib/workloads';
import { LogViewer } from '../components/LogViewer';
import { YamlView } from '../components/YamlView';
import type { WorkloadKind, WorkloadDetail as WorkloadDetailType, PodInfo } from '../lib/types';
import { WORKLOAD_KINDS } from '../lib/types';

export function WorkloadDetail() {
  const { clusterId = '', kind = '', namespace = '', name = '' } = useParams();
  const { user } = useAuth();
  const [tab, setTab] = useState<string | null>('overview');

  if (!WORKLOAD_KINDS.includes(kind as WorkloadKind)) {
    return <Alert color="red" title="Unknown workload kind">{`"${kind}" is not a known workload kind.`}</Alert>;
  }
  const ref: WorkloadRef = { kind: kind as WorkloadKind, namespace, name };

  const { data: cluster } = useCluster(clusterId);
  const ready = !!cluster && clusterUsable(cluster);
  const { data: detail, isLoading, error } = useWorkload(clusterId, ref, ready);
  const pods: PodInfo[] = detail?.pods ?? [];

  const canScale = scalable(ref.kind) && !!cluster && canManageCluster(cluster, user);

  return (
    <>
      <Breadcrumbs mb="sm" separator="›">
        <Anchor component={Link} to={`/workloads?cluster=${clusterId}`} size="sm">
          <Group gap={4} wrap="nowrap">
            <IconChevronLeft size={14} /> Workloads
          </Group>
        </Anchor>
        <Text size="sm" c="dimmed">
          {cluster?.name ?? clusterId}
        </Text>
        <Text size="sm">{name}</Text>
      </Breadcrumbs>

      <Group justify="space-between" mb="lg" align="flex-start">
        <Group gap="sm" align="center">
          <Title order={2}>{name}</Title>
          <Badge color={kindColor(ref.kind)} variant="light">
            {kindLabel(ref.kind)}
          </Badge>
          <Text c="dimmed" size="sm">
            {namespace}
          </Text>
          {detail && (
            <Badge color={statusColor(detail.status)} variant="dot">
              {detail.status}
            </Badge>
          )}
        </Group>
        {canScale && detail && <ScaleButton clusterId={clusterId} workloadRef={ref} current={detail.desired_replicas} />}
      </Group>

      {error ? (
        <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Could not load workload">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !detail ? (
        <Stack>
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} height={80} radius="md" />
          ))}
        </Stack>
      ) : detail ? (
        <Card padding="lg" radius="md">
          <Tabs value={tab} onChange={setTab}>
            <Tabs.List mb="md">
              <Tabs.Tab value="overview">Overview</Tabs.Tab>
              <Tabs.Tab value="pods">Pods ({pods.length})</Tabs.Tab>
              <Tabs.Tab value="logs">Logs</Tabs.Tab>
              <Tabs.Tab value="yaml">YAML</Tabs.Tab>
              <Tabs.Tab value="events">Events</Tabs.Tab>
            </Tabs.List>

            <Tabs.Panel value="overview">
              <Overview detail={detail} />
            </Tabs.Panel>
            <Tabs.Panel value="pods">
              <PodTable pods={pods} />
            </Tabs.Panel>
            <Tabs.Panel value="logs">
              {tab === 'logs' && <LogViewer clusterId={clusterId} workloadRef={ref} pods={pods} />}
            </Tabs.Panel>
            <Tabs.Panel value="yaml">
              {tab === 'yaml' && <YamlTab clusterId={clusterId} workloadRef={ref} name={name} />}
            </Tabs.Panel>
            <Tabs.Panel value="events">
              <EventsTab clusterId={clusterId} workloadRef={ref} enabled={tab === 'events' && ready} />
            </Tabs.Panel>
          </Tabs>
        </Card>
      ) : null}
    </>
  );
}

// ---- Overview ----------------------------------------------------------------

function Overview({ detail }: { detail: WorkloadDetailType }) {
  const stat = (label: string, value: ReactNode) => (
    <div>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
        {label}
      </Text>
      <Text size="lg" fw={600}>
        {value}
      </Text>
    </div>
  );

  const isCron = detail.kind === 'cronjob';
  return (
    <Stack gap="lg">
      <SimpleGrid cols={{ base: 2, sm: 4 }}>
        {isCron
          ? stat('Schedule', <Text ff="monospace" size="md" fw={600}>{detail.schedule ?? '-'}</Text>)
          : stat('Ready', `${detail.ready_replicas}/${detail.desired_replicas}`)}
        {!isCron && stat('Available', detail.available_replicas)}
        {!isCron && stat('Updated', detail.updated_replicas)}
        {stat('Status', detail.status)}
        {detail.strategy && stat('Strategy', detail.strategy)}
      </SimpleGrid>

      <LabelBlock title="Selector" labels={detail.selector} />
      <LabelBlock title="Labels" labels={detail.labels} />

      <div>
        <Text fw={600} mb="xs">
          Containers
        </Text>
        {detail.containers && detail.containers.length > 0 ? (
          <Table.ScrollContainer minWidth={480}>
            <Table withTableBorder>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Name</Table.Th>
                  <Table.Th>Image</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {detail.containers.map((c) => (
                  <Table.Tr key={c.name}>
                    <Table.Td>{c.name}</Table.Td>
                    <Table.Td>
                      <Text ff="monospace" size="sm">
                        {c.image}
                      </Text>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        ) : (
          <Text c="dimmed" size="sm">
            No container information.
          </Text>
        )}
      </div>

      {detail.conditions && detail.conditions.length > 0 && (
        <div>
          <Text fw={600} mb="xs">
            Conditions
          </Text>
          <Table.ScrollContainer minWidth={520}>
            <Table withTableBorder>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Type</Table.Th>
                  <Table.Th>Status</Table.Th>
                  <Table.Th>Reason</Table.Th>
                  <Table.Th>Updated</Table.Th>
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
                    <Table.Td>
                      <Text size="sm" c="dimmed">
                        {c.updated ? relative(c.updated) : '-'}
                      </Text>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </div>
      )}
    </Stack>
  );
}

function LabelBlock({ title, labels }: { title: string; labels?: Record<string, string> }) {
  const entries = Object.entries(labels ?? {});
  if (entries.length === 0) return null;
  return (
    <div>
      <Text fw={600} mb="xs">
        {title}
      </Text>
      <Group gap="xs">
        {entries.map(([k, v]) => (
          <Badge key={k} variant="outline" color="gray" size="sm" radius="sm">
            {k}={v}
          </Badge>
        ))}
      </Group>
    </div>
  );
}

// ---- Pods --------------------------------------------------------------------

function PodTable({ pods }: { pods: PodInfo[] }) {
  if (pods.length === 0) {
    return (
      <Text c="dimmed" size="sm" py="md">
        This workload has no pods.
      </Text>
    );
  }
  return (
    <Table.ScrollContainer minWidth={720}>
      <Table verticalSpacing="sm" highlightOnHover>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Name</Table.Th>
            <Table.Th>Ready</Table.Th>
            <Table.Th>Status</Table.Th>
            <Table.Th>Restarts</Table.Th>
            <Table.Th>Node</Table.Th>
            <Table.Th>IP</Table.Th>
            <Table.Th>Age</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {pods.map((p) => (
            <Table.Tr key={p.name}>
              <Table.Td>
                <Text ff="monospace" size="sm">
                  {p.name}
                </Text>
              </Table.Td>
              <Table.Td>{p.ready}</Table.Td>
              <Table.Td>
                <Badge color={statusColor(p.status)} variant="dot" size="sm">
                  {p.status}
                </Badge>
              </Table.Td>
              <Table.Td>
                <Text c={p.restarts > 0 ? 'orange' : undefined}>{p.restarts}</Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm">{p.node || '-'}</Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm" ff="monospace" c="dimmed">
                  {p.ip || '-'}
                </Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm" c="dimmed">
                  {relative(p.created_at)}
                </Text>
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}

// ---- YAML --------------------------------------------------------------------

function YamlTab({ clusterId, workloadRef, name }: { clusterId: string; workloadRef: WorkloadRef; name: string }) {
  const { data, isLoading, error } = useQuery({
    queryKey: [...qk.workload(clusterId, workloadRef), 'manifest'],
    queryFn: () => api.getWorkloadManifest(clusterId, workloadRef),
    staleTime: 10 * 1000,
  });

  return <YamlView yaml={data} filename={name} isLoading={isLoading} error={error} />;
}

// ---- Events ------------------------------------------------------------------

function EventsTab({ clusterId, workloadRef, enabled }: { clusterId: string; workloadRef: WorkloadRef; enabled: boolean }) {
  const { data: events, isLoading } = useWorkloadEvents(clusterId, workloadRef, enabled);
  if (isLoading && !events) return <Skeleton height={200} radius="md" />;
  if (!events || events.length === 0) {
    return (
      <Text c="dimmed" size="sm" py="md">
        No recent events for this workload.
      </Text>
    );
  }
  return (
    <Table.ScrollContainer minWidth={720}>
      <Table verticalSpacing="sm">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Type</Table.Th>
            <Table.Th>Reason</Table.Th>
            <Table.Th>Message</Table.Th>
            <Table.Th>Object</Table.Th>
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
              <Table.Td>{e.reason}</Table.Td>
              <Table.Td>
                <Text size="sm">{e.message}</Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm" ff="monospace" c="dimmed">
                  {e.object}
                </Text>
              </Table.Td>
              <Table.Td>{e.count}</Table.Td>
              <Table.Td>
                <Text size="sm" c="dimmed">
                  {relative(e.last_seen)}
                </Text>
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}

// ---- Scale -------------------------------------------------------------------

function ScaleButton({ clusterId, workloadRef, current }: { clusterId: string; workloadRef: WorkloadRef; current: number }) {
  const [opened, { open, close }] = useDisclosure(false);
  const [value, setValue] = useState<number | string>(current);
  const scale = useScaleWorkload(clusterId);

  const apply = () => {
    const n = typeof value === 'number' ? value : parseInt(value, 10);
    if (Number.isNaN(n) || n < 0) {
      notifications.show({ color: 'red', title: 'Invalid replica count', message: 'Enter a number ≥ 0.' });
      return;
    }
    scale.mutate({ ref: workloadRef, replicas: n }, { onSuccess: close });
  };

  return (
    <>
      <Button variant="light" leftSection={<IconArrowsMaximize size={16} />} onClick={() => { setValue(current); open(); }}>
        Scale
      </Button>
      <Modal opened={opened} onClose={close} title={`Scale ${workloadRef.name}`} centered>
        <Stack>
          <Text size="sm" c="dimmed">
            Set the desired replica count. The control plane applies it directly to the cluster.
          </Text>
          <NumberInput
            label="Replicas"
            min={0}
            max={100}
            value={value}
            onChange={setValue}
            allowDecimal={false}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>
              Cancel
            </Button>
            <Button onClick={apply} loading={scale.isPending}>
              Apply
            </Button>
          </Group>
        </Stack>
      </Modal>
    </>
  );
}
