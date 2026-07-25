import { useEffect, useState, type ReactNode } from 'react';
import { Link, useNavigate, useParams } from 'react-router';
import {
  Group,
  Title,
  Text,
  Button,
  Card,
  Tabs,
  Stack,
  Badge,
  Skeleton,
  Table,
  Code,
  Breadcrumbs,
  Anchor,
  ActionIcon,
  Tooltip,
  Alert,
  SimpleGrid,
  CopyButton,
  Divider,
  Paper,
  ThemeIcon,
} from '@mantine/core';
import { modals } from '@mantine/modals';
import { notifications } from '@mantine/notifications';
import {
  IconServer2,
  IconDownload,
  IconTrash,
  IconRefresh,
  IconInfoCircle,
  IconArrowUpCircle,
  IconShieldCheck,
  IconCloud,
  IconCopy,
  IconCheck,
  IconTerminal2,
  IconEye,
  IconListSearch,
  IconVersions,
  IconServer,
  IconServerBolt,
  IconPackage,
  IconSitemap,
  IconNetwork,
  type Icon as TablerIcon,
} from '@tabler/icons-react';
import { useCluster, useCatalog, useUpgrades, useDeleteCluster, useUpgradeCluster, useOperations, useMetrics, useHealth } from '../lib/queries';
import { api } from '../lib/api';
import { useClusterEvents } from '../lib/events';
import { ClusterStatusBadge } from '../components/ClusterStatusBadge';
import { HealthBadge } from '../components/HealthBadge';
import { HealthPanel } from '../components/HealthPanel';
import { PhaseStepper, GenerationHint } from '../components/PhaseStepper';
import { NodeTable } from '../components/NodeTable';
import { NodeDetailPane } from '../components/NodeDetailPane';
import { NodeSshModal } from '../components/NodeSshModal';
import { ClusterUsageCard } from '../components/ClusterUsageCard';
import { ActivityTimeline } from '../components/ActivityTimeline';
import { OperationList } from '../components/OperationList';
import { UpgradeProgress } from '../components/UpgradeProgress';
import { WorkerScaleProgress } from '../components/WorkerScaleProgress';
import { ManagePanel } from '../components/ManagePanel';
import { NodePoolsPanel } from '../components/NodePoolsPanel';
import { ClusterShell } from '../components/ClusterShell';
import { AuditPanel } from '../components/AuditPanel';
import { EmptyState } from '../components/EmptyState';
import { addonColor } from '../lib/phase';
import { relative } from '../lib/format';
import { downloadText } from '../lib/download';
import {
  canManageCluster,
  controlPlaneCount,
  isHA,
  apiEndpoint,
  provisionedNodeCount,
  desiredNodeCount,
  workerCount,
  networkGateway,
  clusterProvider,
  providerLabel,
  clusterUsable,
} from '../lib/cluster';
import { useAuth } from '../lib/auth';
import type { Cluster, Bundle, Catalog, Operation, HealthSnapshot } from '../lib/types';

export function ClusterDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { user } = useAuth();
  const { data: cluster, isLoading, isError } = useCluster(id);
  const { data: catalog } = useCatalog();
  const { events, status } = useClusterEvents(id);
  const { data: operations } = useOperations(id);
  // Live usage is only meaningful once the cluster has a reachable control plane and metrics-server
  // is installed; gating the query keeps us from polling for 204s on every other cluster. Updating
  // counts as reachable (see clusterUsable) - an add-on edit or node resize shouldn't yank this away.
  const metricsEnabled =
    !!cluster &&
    clusterUsable(cluster) &&
    (cluster.addons ?? []).some((a) => a.name === 'metrics-server' && a.phase === 'installed');
  const { data: metrics } = useMetrics(id, !!metricsEnabled);
  // Health is evaluated worker-side only for a reachable cluster (matching the backend ticker), so
  // gate the poll the same way to avoid asking for 204s during initial bring-up.
  const healthReady = !!cluster && clusterUsable(cluster);
  const { data: health } = useHealth(id, !!healthReady);
  const del = useDeleteCluster();
  const [tab, setTab] = useState<string | null>('overview');
  // Once the shell tab is opened we keep its panel mounted (see keepMounted below) so switching
  // tabs doesn't tear down the WebSocket/PTY session. We still gate the first mount on an actual
  // visit so a page load doesn't eagerly open a PTY the user may never use.
  const [shellVisited, setShellVisited] = useState(false);
  useEffect(() => {
    if (tab === 'shell') setShellVisited(true);
  }, [tab]);
  const [downloading, setDownloading] = useState(false);
  // The node whose detail pane is open. Held by VM NAME rather than the node object so the pane
  // re-reads from the polled cluster on every refresh - otherwise a disk added from inside it
  // wouldn't show up until the pane was closed and reopened.
  const [selectedNode, setSelectedNode] = useState<string | null>(null);
  // The node whose SSH modal is open - likewise held by VM NAME so the modal re-reads from polled
  // state (e.g. an IP that arrives while provisioning), same reasoning as selectedNode.
  const [sshNode, setSshNode] = useState<string | null>(null);

  if (isLoading) {
    return (
      <Stack maw={1000}>
        <Skeleton height={40} width={280} />
        <Skeleton height={90} />
        <Skeleton height={300} />
      </Stack>
    );
  }
  if (isError || !cluster) {
    return (
      <EmptyState
        icon={IconServer2}
        title="Cluster not found"
        description="It may have been deleted."
        action={
          <Button component={Link} to="/clusters" variant="light" mt="sm">
            Back to clusters
          </Button>
        }
      />
    );
  }

  const downloadKubeconfig = async () => {
    setDownloading(true);
    try {
      const kc = await api.getKubeconfig(cluster.id);
      // Read-role members receive the read-only viewer kubeconfig; name it so the two don't collide.
      const suffix = canManage ? '' : '-readonly';
      downloadText(`${cluster.name}${suffix}.kubeconfig`, kc, 'application/yaml');
    } catch (err) {
      notifications.show({
        color: 'red',
        title: 'Kubeconfig unavailable',
        message: err instanceof Error ? err.message : String(err),
      });
    } finally {
      setDownloading(false);
    }
  };

  const confirmDelete = () =>
    modals.openConfirmModal({
      title: `Delete ${cluster.name}?`,
      centered: true,
      children: <Text size="sm">This tears down the cluster and all of its VMs. This cannot be undone.</Text>,
      labels: { confirm: 'Delete cluster', cancel: 'Cancel' },
      confirmProps: { color: 'red' },
      onConfirm: () => del.mutate(cluster.id, { onSuccess: () => navigate('/clusters') }),
    });

  const addons = cluster.addons ?? [];
  // Write access gate: owner, admin, or a Write-role group-mate. A read-only group member sees the
  // cluster but every mutating control below is hidden/disabled (the server enforces this too).
  const canManage = canManageCluster(cluster, user);

  return (
    <>
      <Breadcrumbs mb="xs">
        <Anchor component={Link} to="/clusters" size="sm">
          Clusters
        </Anchor>
        <Text size="sm" c="dimmed">
          {cluster.name}
        </Text>
      </Breadcrumbs>

      <Group justify="space-between" align="flex-start" mb="md" gap="sm">
        <Group gap="sm">
          <Title order={2}>{cluster.name}</Title>
          <ClusterStatusBadge phase={cluster.phase} size="md" />
          {healthReady && health && <HealthBadge status={health.status} size="md" />}
          {isHA(cluster) && (
            <Badge color="grape" variant="light" leftSection={<IconShieldCheck size={12} />}>
              HA
            </Badge>
          )}
          {clusterProvider(cluster) === 'vsphere' && (
            <Badge color="indigo" variant="light" leftSection={<IconCloud size={12} />}>
              vSphere
            </Badge>
          )}
          {!canManage && (
            <Tooltip label="You have the Read role in this group - you can view this cluster but not manage it">
              <Badge color="gray" variant="light" leftSection={<IconEye size={12} />}>
                read-only
              </Badge>
            </Tooltip>
          )}
        </Group>
        <Group gap="xs" wrap="nowrap">
          <Tooltip
            label={
              !clusterUsable(cluster)
                ? 'Available once Ready'
                : canManage
                  ? 'Download admin kubeconfig'
                  : 'Download read-only kubeconfig (view access - no secrets, no mutations)'
            }
          >
            <Button
              variant="default"
              leftSection={<IconDownload size={16} />}
              onClick={downloadKubeconfig}
              loading={downloading}
              disabled={!clusterUsable(cluster)}
            >
              {canManage ? 'Kubeconfig' : 'Read-only kubeconfig'}
            </Button>
          </Tooltip>
          {canManage && (
            <Tooltip label="Delete cluster">
              <ActionIcon variant="light" color="red" size={36} onClick={confirmDelete} disabled={cluster.phase === 'Deleting'}>
                <IconTrash size={18} />
              </ActionIcon>
            </Tooltip>
          )}
        </Group>
      </Group>

      <Card radius="md" padding="lg" mb="md">
        <Group justify="space-between" mb="md">
          <Text fw={600}>Provisioning progress</Text>
          <GenerationHint cluster={cluster} />
        </Group>
        <PhaseStepper cluster={cluster} />
      </Card>

      {metricsEnabled && <ClusterUsageCard snapshot={metrics} />}

      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List mb="md" style={{ flexWrap: 'nowrap', overflowX: 'auto', overflowY: 'hidden' }}>
          <Tabs.Tab value="overview">Overview</Tabs.Tab>
          <Tabs.Tab value="nodes">Nodes ({provisionedNodeCount(cluster)})</Tabs.Tab>
          <Tabs.Tab value="shell" leftSection={<IconTerminal2 size={14} />}>
            Terminal
          </Tabs.Tab>
          <Tabs.Tab value="addons">Add-ons ({addons.length})</Tabs.Tab>
          <Tabs.Tab value="activity" leftSection={<IconRefresh size={14} />}>
            Activity
          </Tabs.Tab>
          <Tabs.Tab value="audit" leftSection={<IconListSearch size={14} />}>
            Audit
          </Tabs.Tab>
          <Tabs.Tab value="upgrades">Upgrades</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="overview">
          <OverviewTab cluster={cluster} health={healthReady ? health : undefined} />
        </Tabs.Panel>

        <Tabs.Panel value="nodes">
          <Stack gap="md">
            <WorkerScaleProgress
              cluster={cluster}
              op={(operations ?? []).find((o) => o.kind === 'scale' && o.status === 'in_progress')}
            />
            <Card radius="md" padding="lg">
              <Text fw={600} mb="sm">
                Node pools
              </Text>
              <NodePoolsPanel cluster={cluster} canManage={canManage} />
            </Card>
            <Card radius="md" padding="lg">
              <NodeTable
                nodes={cluster.nodes}
                metrics={metrics?.nodes}
                health={health?.nodes}
                disks={cluster.node_disks}
                onSelect={(n) => setSelectedNode(n.vm_name)}
                // SSH is a write action - the column and pane button appear only for managers (the
                // API is the authoritative gate). setSshNode by VM name so the modal tracks polled state.
                onSsh={canManage ? (n) => setSshNode(n.vm_name) : undefined}
              />
            </Card>
            {/* Per-node settings live in a right-hand pane rather than in the table: the table has
                to stay scannable across a whole cluster, and this is where later per-node knobs go. */}
            <NodeDetailPane
              cluster={cluster}
              node={(cluster.nodes ?? []).find((n) => n.vm_name === selectedNode) ?? null}
              onClose={() => setSelectedNode(null)}
              onSsh={canManage ? (n) => setSshNode(n.vm_name) : undefined}
            />
            <NodeSshModal
              cluster={cluster}
              node={(cluster.nodes ?? []).find((n) => n.vm_name === sshNode) ?? null}
              onClose={() => setSshNode(null)}
            />
          </Stack>
        </Tabs.Panel>

        <Tabs.Panel value="shell" keepMounted={shellVisited}>
          {clusterUsable(cluster) ? (
            <Card radius="md" padding="lg">
              {canManage ? (
                <Text size="sm" c="dimmed" mb="sm">
                  An interactive shell with <Code>kubectl</Code> configured for this cluster. Try{' '}
                  <Code>kubectl get nodes</Code>.
                </Text>
              ) : (
                <Alert
                  color="gray"
                  variant="light"
                  icon={<IconEye size={16} />}
                  title="Read-only session"
                  mb="sm"
                >
                  You have view access to this cluster, so this shell uses a read-only kubeconfig:{' '}
                  <Code>kubectl get</Code> works, but mutating commands (and reading secrets) are
                  rejected by the cluster.
                </Alert>
              )}
              <ClusterShell id={cluster.id} readOnly={!canManage} />
            </Card>
          ) : (
            <Card radius="md" padding="lg">
              <EmptyState
                icon={IconTerminal2}
                title="Shell available once Ready"
                description="The in-browser kubectl terminal opens as soon as the cluster finishes provisioning."
              />
            </Card>
          )}
        </Tabs.Panel>

        <Tabs.Panel value="addons">
          <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
            <Card radius="md" padding="lg">
              <Text fw={600} mb="sm">
                Installed
              </Text>
              {addons.length === 0 ? (
                <Text c="dimmed" size="sm">
                  No add-ons.
                </Text>
              ) : (
                <Table verticalSpacing="xs">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Name</Table.Th>
                      <Table.Th>Version</Table.Th>
                      <Table.Th>Phase</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {addons.map((a) => (
                      <Table.Tr key={a.name}>
                        <Table.Td>
                          <Group gap={6}>
                            {a.name}
                            {a.catalog_id && (
                              <Badge size="xs" variant="light" color="brand">
                                custom
                              </Badge>
                            )}
                          </Group>
                        </Table.Td>
                        <Table.Td>
                          <Code>{a.version}</Code>
                        </Table.Td>
                        <Table.Td>
                          <Badge size="sm" variant="dot" color={addonColor(a.phase)}>
                            {a.phase}
                          </Badge>
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              )}
              <Text size="xs" c="dimmed" mt="sm">
                CNI {cluster.cni} {cluster.cni_version} is installed as part of the bundle.
              </Text>
            </Card>
            <Card radius="md" padding="lg">
              <Text fw={600} mb="sm">
                Manage
              </Text>
              <ManagePanel cluster={cluster} catalog={catalog} canManage={canManage} />
            </Card>
          </SimpleGrid>
        </Tabs.Panel>

        <Tabs.Panel value="activity">
          <Stack gap="md">
            <Card radius="md" padding="lg">
              <ActivityTimeline events={events} status={status} />
            </Card>
            <Card radius="md" padding="lg">
              <Text fw={600} mb="md">
                Operations history
              </Text>
              <OperationList
                operations={operations ?? []}
                emptyLabel="No operations yet - actions like scaling, add-on changes, and upgrades will appear here."
              />
            </Card>
          </Stack>
        </Tabs.Panel>

        <Tabs.Panel value="audit">
          {clusterUsable(cluster) ? (
            <Card radius="md" padding="lg">
              <AuditPanel clusterId={cluster.id} enabled={clusterUsable(cluster)} />
            </Card>
          ) : (
            <Card radius="md" padding="lg">
              <EmptyState
                icon={IconListSearch}
                title="Audit available once Ready"
                description="API-server audit logging is enabled on every cluster by default; the event feed opens as soon as the cluster finishes provisioning."
              />
            </Card>
          )}
        </Tabs.Panel>

        <Tabs.Panel value="upgrades">
          <UpgradesTab cluster={cluster} catalog={catalog} operations={operations ?? []} canManage={canManage} />
        </Tabs.Panel>
      </Tabs>
    </>
  );
}

// A labelled row in a spec card: dimmed label on the left, value right-aligned. The value is passed
// as children so it can be plain text, a <Code> chip, a badge, or a CopyValue.
function KV({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Group justify="space-between" wrap="nowrap" gap="xl" align="flex-start">
      <Text size="sm" c="dimmed" style={{ flexShrink: 0 }}>
        {label}
      </Text>
      <Text size="sm" fw={500} ta="right" component="div" style={{ minWidth: 0 }}>
        {children}
      </Text>
    </Group>
  );
}

// A consistent icon + title header for the overview spec cards, matching HealthPanel /
// ClusterUsageCard so every card on the tab reads the same way.
function SpecHeader({
  icon: Icon,
  title,
  color = 'gray',
}: {
  icon: TablerIcon;
  title: string;
  color?: string;
}) {
  return (
    <Group gap={8} mb="md">
      <ThemeIcon size={26} radius="md" variant="light" color={color}>
        <Icon size={15} stroke={1.7} />
      </ThemeIcon>
      <Text fw={600}>{title}</Text>
    </Group>
  );
}

// A monospace value with an inline copy affordance - for the technical identifiers on the tab
// (cluster ID, endpoint, CIDRs) that a user is likely to paste elsewhere.
function CopyValue({ value }: { value: string }) {
  return (
    <Group gap={4} wrap="nowrap" justify="flex-end">
      <Code>{value}</Code>
      <CopyButton value={value}>
        {({ copied, copy }) => (
          <Tooltip label={copied ? 'Copied' : 'Copy'}>
            <ActionIcon size="xs" variant="subtle" color={copied ? 'teal' : 'gray'} onClick={copy}>
              {copied ? <IconCheck size={12} /> : <IconCopy size={12} />}
            </ActionIcon>
          </Tooltip>
        )}
      </CopyButton>
    </Group>
  );
}

// A compact headline tile for the Overview tab's at-a-glance strip: a coloured icon beside a small
// uppercase label, a bold value, and a dimmed sub-line. Values can be long strings (e.g. "VMware
// vSphere"), so both value and sub truncate rather than wrap the tile.
function FactTile({
  icon: Icon,
  label,
  value,
  sub,
  color = 'brand',
}: {
  icon: TablerIcon;
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  color?: string;
}) {
  return (
    <Paper p="md" radius="md">
      <Group gap="sm" wrap="nowrap" align="center">
        <ThemeIcon size={40} radius="md" variant="light" color={color}>
          <Icon size={21} stroke={1.6} />
        </ThemeIcon>
        <div style={{ minWidth: 0 }}>
          <Text size="xs" c="dimmed" fw={600} tt="uppercase" lts={0.3}>
            {label}
          </Text>
          <Text fw={700} fz="lg" lh={1.2} truncate>
            {value}
          </Text>
          {sub && (
            <Text size="xs" c="dimmed" truncate>
              {sub}
            </Text>
          )}
        </div>
      </Group>
    </Paper>
  );
}

function OverviewTab({ cluster, health }: { cluster: Cluster; health?: HealthSnapshot | null }) {
  const endpoint = apiEndpoint(cluster);
  const isReady = clusterUsable(cluster);
  const provider = clusterProvider(cluster);
  const vsphere = provider === 'vsphere';
  const ha = isHA(cluster);
  const cps = controlPlaneCount(cluster);
  const workers = workerCount(cluster);
  const provisioned = provisionedNodeCount(cluster);
  const desired = desiredNodeCount(cluster);
  const pools = cluster.node_pools ?? [];
  // vSphere addressing mode is the most useful sub-line for the infra tile; on kvm it's always a
  // dedicated NAT bridge.
  const netMode = vsphere
    ? cluster.ip_mode === 'static'
      ? 'static · shared portgroup'
      : 'DHCP · shared portgroup'
    : 'NAT · dedicated bridge';

  return (
    <Stack gap="md">
      {/* At-a-glance headline facts. The detail cards below carry the full spec sheet. */}
      <SimpleGrid cols={{ base: 2, md: 4 }} spacing="md">
        <FactTile
          icon={IconVersions}
          color="brand"
          label="Kubernetes"
          value={cluster.k8s_version}
          sub={cluster.bundle}
        />
        <FactTile
          icon={IconServer2}
          color="teal"
          label="Nodes"
          value={`${provisioned} / ${desired}`}
          sub={`${cps} control plane · ${workers} worker`}
        />
        <FactTile
          icon={ha ? IconShieldCheck : IconServer}
          color={ha ? 'grape' : 'gray'}
          label="Control plane"
          value={ha ? `HA · ${cps} nodes` : 'Single node'}
          sub={`${cluster.size} size`}
        />
        <FactTile
          icon={vsphere ? IconCloud : IconServerBolt}
          color={vsphere ? 'indigo' : 'teal'}
          label="Infrastructure"
          value={providerLabel(provider)}
          sub={netMode}
        />
      </SimpleGrid>

      <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
        <Card radius="md" padding="lg">
          <SpecHeader icon={IconPackage} title="Version provenance" color="brand" />
          <Stack gap={10}>
            <KV label="Release bundle">
              <Badge variant="light" color="brand" size="sm" radius="sm">
                {cluster.bundle}
              </Badge>
            </KV>
            <KV label="Kubernetes">
              <Code>{cluster.k8s_version}</Code>
            </KV>
            <KV label="OS image">
              <Code>{cluster.os_image}</Code>
            </KV>
            <KV label="CNI">
              <Code>
                {cluster.cni} {cluster.cni_version}
              </Code>
            </KV>
          </Stack>
        </Card>

        <Card radius="md" padding="lg">
          <SpecHeader icon={IconSitemap} title="Topology" color="grape" />
          <Stack gap={10}>
            <KV label="Cluster ID">
              <CopyValue value={cluster.id} />
            </KV>
            <KV label="Control plane">
              {ha ? (
                <Badge color="grape" variant="light" size="sm" leftSection={<IconShieldCheck size={12} />}>
                  HA · {cps} nodes
                </Badge>
              ) : (
                <Badge color="gray" variant="light" size="sm">
                  single node
                </Badge>
              )}
            </KV>
            <KV label="Control plane size">
              <Badge variant="default" size="sm" radius="sm">
                {cluster.size}
              </Badge>
            </KV>
            <KV label="Node pools">
              {pools.length === 0 ? (
                <Text span c="dimmed">
                  -
                </Text>
              ) : (
                <Group gap={6} justify="flex-end">
                  {pools.map((p) => (
                    <Badge key={p.name} variant="light" color="gray" size="sm" radius="sm">
                      {p.name}: {p.desired_workers} × {p.size}
                    </Badge>
                  ))}
                </Group>
              )}
            </KV>
            <Divider my={2} />
            <KV label="Requested by">{cluster.owner_username}</KV>
            <KV label="Created">{relative(cluster.created_at)}</KV>
          </Stack>
        </Card>

        <Card radius="md" padding="lg">
          <SpecHeader icon={IconNetwork} title="Networking" color="cyan" />
          <Stack gap={10}>
            {cluster.network_cidr && (
              <>
                <KV label="Node network">
                  <CopyValue value={cluster.network_cidr} />
                </KV>
                {/* On vSphere the nodes sit on the operator's shared portgroup: the gateway is
                    configuration (static mode) rather than the .1-by-convention libvirt bridge, and
                    in DHCP mode it isn't ours to know at all. */}
                {vsphere ? (
                  <>
                    {cluster.network_name && (
                      <KV label="Portgroup">
                        <Code>{cluster.network_name}</Code>
                      </KV>
                    )}
                    {cluster.net_gateway && (
                      <KV label="Gateway">
                        <Code>{cluster.net_gateway}</Code>
                      </KV>
                    )}
                    <KV label="Mode">
                      <Badge variant="light" color="indigo" size="sm">
                        {cluster.ip_mode === 'static' ? 'static · shared portgroup' : 'DHCP · shared portgroup'}
                      </Badge>
                    </KV>
                  </>
                ) : (
                  <>
                    <KV label="Gateway">
                      <Code>{networkGateway(cluster.network_cidr)}</Code>
                    </KV>
                    <KV label="Mode">
                      <Badge variant="light" color="teal" size="sm">
                        NAT · dedicated bridge
                      </Badge>
                    </KV>
                  </>
                )}
                {cluster.api_vip && (
                  <KV label="API VIP">
                    <Code>{cluster.api_vip}</Code>
                  </KV>
                )}
                {cluster.load_balancer_ip && (
                  <KV label="LoadBalancer IP">
                    <CopyValue value={cluster.load_balancer_ip} />
                  </KV>
                )}
                {cluster.apps_domain && (
                  /* Every name under here resolves to the LoadBalancer IP above: expose an app on
                     <anything>.apps.<cluster>.<domain> and routing works with nothing to configure. */
                  <KV label="Apps domain">
                    <CopyValue value={`*.${cluster.apps_domain}`} />
                  </KV>
                )}
                {endpoint && (
                  <KV label="Endpoint">
                    <CopyValue value={endpoint} />
                  </KV>
                )}
                <Divider my={2} />
              </>
            )}
            <KV label="Pod CIDR">
              <Code>{cluster.pod_cidr}</Code>
            </KV>
            <KV label="Service CIDR">
              <Code>{cluster.svc_cidr}</Code>
            </KV>
          </Stack>
        </Card>

        {isReady ? (
          // The health panel is a grid cell like the others, so it matches their width; it folds in
          // the rolled-up status string as its footer.
          <HealthPanel snapshot={health} status={cluster.status} />
        ) : (
          cluster.status && (
            <Card radius="md" padding="lg">
              <SpecHeader icon={IconInfoCircle} title="Status" color="gray" />
              <Text size="sm" c="dimmed">
                {cluster.status}
              </Text>
            </Card>
          )
        )}
      </SimpleGrid>
    </Stack>
  );
}

// A single component that changes when promoting to a target bundle, plus the strategy the
// reconciler uses to converge it (mirrors internal/reconcile.reconcileUpgrade).
interface BundleChange {
  label: string;
  from: string;
  to: string;
  strategy: string;
}

function bundleChanges(c: Cluster, b: Bundle): BundleChange[] {
  const out: BundleChange[] = [];
  if (c.os_image !== b.os) {
    out.push({ label: 'OS', from: c.os_image, to: b.os, strategy: 'rolling node replacement' });
  }
  if (c.k8s_version !== b.kubernetes) {
    out.push({ label: 'Kubernetes', from: c.k8s_version, to: b.kubernetes, strategy: 'in-place kubeadm upgrade' });
  }
  const targetCni = b.addons[b.cni];
  if (c.cni !== b.cni || c.cni_version !== targetCni) {
    out.push({ label: `CNI (${b.cni})`, from: `${c.cni} ${c.cni_version}`, to: `${b.cni} ${targetCni}`, strategy: 'helm upgrade' });
  }
  const current = new Map(
    (c.addons ?? []).filter((a) => a.phase !== 'removing').map((a) => [a.name, a.version] as const),
  );
  for (const [name, ver] of Object.entries(b.addons)) {
    if (name === b.cni) continue;
    const cur = current.get(name);
    if (cur !== undefined && cur !== ver) {
      out.push({ label: `add-on ${name}`, from: cur, to: ver, strategy: 'helm upgrade' });
    }
  }
  return out;
}

function UpgradesTab({
  cluster,
  catalog,
  operations,
  canManage,
}: {
  cluster: Cluster;
  catalog?: Catalog;
  operations: Operation[];
  canManage: boolean;
}) {
  const { data: upgrades, isLoading } = useUpgrades(cluster.id);
  const upgrade = useUpgradeCluster(cluster.id);

  const upgrading = cluster.phase === 'Upgrading' || (!!cluster.target_bundle && cluster.target_bundle !== cluster.bundle);
  const ready = cluster.phase === 'Ready';

  // The final target bundle and the in-progress upgrade record drive the live progress view.
  const targetBundle = catalog?.bundles.find((b) => b.name === cluster.target_bundle);
  const activeUpgradeOp = operations.find((o) => o.kind === 'upgrade' && o.status === 'in_progress');
  const upgradeHistory = operations.filter((o) => o.kind === 'upgrade');

  const confirmUpgrade = (bundle: string, changes: BundleChange[]) =>
    modals.openConfirmModal({
      title: `Upgrade ${cluster.name} to ${bundle}?`,
      centered: true,
      children: (
        <Stack gap="xs">
          <Text size="sm">The reconciler will converge the running cluster to this bundle:</Text>
          {changes.map((ch) => (
            <Text key={ch.label} size="sm">
              • <b>{ch.label}</b>: <Code>{ch.from}</Code> → <Code>{ch.to}</Code>{' '}
              <Text span c="dimmed" size="xs">
                ({ch.strategy})
              </Text>
            </Text>
          ))}
        </Stack>
      ),
      labels: { confirm: `Upgrade to ${bundle}`, cancel: 'Cancel' },
      confirmProps: { color: 'cyan' },
      onConfirm: () => upgrade.mutate(bundle),
    });

  if (isLoading) return <Skeleton height={120} radius="md" />;

  return (
    <Stack>
      {upgrading &&
        (targetBundle ? (
          <UpgradeProgress cluster={cluster} target={targetBundle} op={activeUpgradeOp} />
        ) : (
          <Alert variant="light" color="cyan" icon={<IconArrowUpCircle size={16} />}>
            Upgrade in progress{cluster.target_bundle ? ` toward ${cluster.target_bundle}` : ''}. The
            reconciler is converging the cluster one hop (and, for an OS change, one node) at a time -
            follow it live in the <b>Activity</b> tab.
          </Alert>
        ))}

      {(!upgrades || upgrades.length === 0) && !upgrading ? (
        <Card radius="md" padding="lg">
          <EmptyState
            icon={IconArrowUpCircle}
            title="Up to date"
            description="No newer release bundle is available to promote this cluster to."
          />
        </Card>
      ) : (
        (upgrades ?? []).length > 0 && (
          <Card radius="md" padding="lg">
            <Text fw={600} mb="xs">
              Available upgrades
            </Text>
            {!ready && !upgrading && (
              <Alert variant="light" color="gray" icon={<IconInfoCircle size={16} />} mb="md">
                Upgrades can be started once the cluster is <b>Ready</b>.
              </Alert>
            )}
            {!canManage && (
              <Alert variant="light" color="gray" icon={<IconEye size={16} />} mb="md">
                Read-only access - upgrades can be started by owners, group members with the{' '}
                <b>Write</b> role, and admins.
              </Alert>
            )}
            <Stack gap="sm">
              {(upgrades ?? []).map((b) => {
                const changes = bundleChanges(cluster, b);
                const osChange = changes.some((ch) => ch.label === 'OS');
                const soleCPOutage = osChange && !isHA(cluster);
                return (
                  <Card key={b.name} radius="md" padding="md" withBorder>
                    <Group justify="space-between" align="flex-start" wrap="nowrap">
                      <div style={{ minWidth: 0 }}>
                        <Group gap="xs" mb={6}>
                          <IconArrowUpCircle size={18} />
                          <Text fw={600} size="sm">
                            {b.name}
                          </Text>
                          <Badge size="xs" variant="light" color={b.status === 'supported' ? 'teal' : 'gray'}>
                            {b.status}
                          </Badge>
                        </Group>
                        <Stack gap={4}>
                          {changes.length === 0 ? (
                            <Text size="xs" c="dimmed">
                              No component changes.
                            </Text>
                          ) : (
                            changes.map((ch) => (
                              <Text key={ch.label} size="xs" c="dimmed">
                                <b>{ch.label}</b> <Code>{ch.from}</Code> → <Code>{ch.to}</Code> · {ch.strategy}
                              </Text>
                            ))
                          )}
                          {soleCPOutage && (
                            <Text size="xs" c="orange">
                              Single control plane: rebuilt via etcd backup/restore onto the same IP -
                              expect a brief API outage during the swap.
                            </Text>
                          )}
                        </Stack>
                      </div>
                      <Button
                        size="xs"
                        color="cyan"
                        variant="light"
                        leftSection={<IconArrowUpCircle size={14} />}
                        disabled={!ready || upgrading || !canManage}
                        loading={upgrade.isPending}
                        onClick={() => confirmUpgrade(b.name, changes)}
                      >
                        Upgrade
                      </Button>
                    </Group>
                  </Card>
                );
              })}
            </Stack>
          </Card>
        )
      )}

      <Card radius="md" padding="lg">
        <Text fw={600} mb="md">
          Upgrade history
        </Text>
        <OperationList
          operations={upgradeHistory}
          kinds={['upgrade']}
          emptyLabel="No upgrades yet. Past bundle promotions will be recorded here with their version changes."
        />
      </Card>
    </Stack>
  );
}
