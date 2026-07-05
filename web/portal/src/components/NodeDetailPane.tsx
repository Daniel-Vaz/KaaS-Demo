import { useState } from 'react';
import {
  Drawer,
  Stack,
  Group,
  Text,
  Badge,
  Code,
  Divider,
  Button,
  Table,
  ActionIcon,
  Tooltip,
  TextInput,
  NumberInput,
  Select,
  Alert,
  Paper,
  ThemeIcon,
  SegmentedControl,
} from '@mantine/core';
import { modals } from '@mantine/modals';
import {
  IconServer,
  IconServerBolt,
  IconDeviceSdCard,
  IconTrash,
  IconPlus,
  IconInfoCircle,
  IconTerminal2,
} from '@tabler/icons-react';
import { useAddNodeDisk, useRemoveNodeDisk } from '../lib/queries';
import { nodeColor } from '../lib/phase';
import {
  DISK_FS_TYPES,
  MIN_DISK_GB,
  MAX_DISK_GB,
  PROTECTED_MOUNTS,
  feedsStoragePool,
  isPlatformStorageDisk,
  longhornMountPath,
  type Cluster,
  type Node,
  type NodeDisk,
} from '../lib/types';

// NodeDetailPane is the per-node settings surface: click a node in the table and it slides in from
// the right. Today it holds the node's identity and its extra disks; it is deliberately shaped as a
// general home for per-node configuration, so later knobs land here as new sections rather than as
// more columns in the node table (which has to stay scannable across a whole cluster).

// diskPhaseColor mirrors the reconciler's disk lifecycle (domain.NodeDisk): pending is in-flight,
// attached is the steady state, removing is on its way out.
function diskPhaseColor(phase: NodeDisk['phase']): string {
  switch (phase) {
    case 'attached':
      return 'teal';
    case 'removing':
      return 'orange';
    default:
      return 'blue';
  }
}

// validMountPath mirrors domain.ValidateMountPath. The server is the authoritative gate - this only
// spares the user a round-trip and, more usefully, explains WHY before they hit Add.
function mountPathError(path: string): string | null {
  if (!path) return null; // not an error yet - just not filled in
  if (!path.startsWith('/')) return 'Must be an absolute path';
  if (path !== '/' && path.endsWith('/')) return 'No trailing slash';
  if (path.includes('..') || path.includes('./')) return 'Must be a clean path';
  if (PROTECTED_MOUNTS.includes(path)) {
    return 'A system directory - a new filesystem here would hide the running system';
  }
  return null;
}

function diskNameError(name: string): string | null {
  if (!name) return null;
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name)) {
    return 'Lowercase letters, digits and dashes only';
  }
  if (name.length > 32) return 'Too long (max 32)';
  return null;
}

// AddDiskForm is the inline "attach a disk" form. It stays collapsed until asked for, so the common
// case (looking at a node) isn't dominated by a form.
function AddDiskForm({
  cluster,
  node,
  onDone,
}: {
  cluster: Cluster;
  node: Node;
  onDone: () => void;
}) {
  const add = useAddNodeDisk(cluster.id);
  const [name, setName] = useState('data');
  const [sizeGB, setSizeGB] = useState<number>(20);
  // "pool" is the default and the reason most disks are attached: the disk joins the cluster's
  // Longhorn storage pool, which means it is simply mounted under the Longhorn data path - so the
  // path is derived from the name rather than asked for. "mount" is the escape hatch: an ordinary
  // filesystem at a path of the user's choosing, which Longhorn ignores.
  const [use, setUse] = useState<'pool' | 'mount'>('pool');
  const [mountPath, setMountPath] = useState('/var/lib/data');
  const [fsType, setFsType] = useState<string>('ext4');

  const pool = use === 'pool';
  const path = pool ? longhornMountPath(name || 'disk') : mountPath;
  const nameErr = diskNameError(name);
  const mountErr = pool ? null : mountPathError(mountPath);
  const taken = (cluster.node_disks ?? []).some(
    (d) => d.vm_name === node.vm_name && d.name === name,
  );
  const ready = !!name && !!path && !nameErr && !mountErr && !taken && sizeGB >= MIN_DISK_GB;

  return (
    <Paper withBorder p="sm" radius="md">
      <Stack gap="sm">
        <SegmentedControl
          size="xs"
          fullWidth
          value={use}
          onChange={(v) => setUse(v as 'pool' | 'mount')}
          data={[
            { value: 'pool', label: 'Storage pool' },
            { value: 'mount', label: 'Mounted filesystem' },
          ]}
        />
        <Text size="xs" c="dimmed">
          {pool
            ? `Adds capacity to this node's share of the cluster's Longhorn storage pool - more room for PersistentVolumes, with nothing to configure. It is mounted at ${path}.`
            : 'An ordinary filesystem mounted where you choose, for a workload that wants the disk directly. Longhorn does not use it.'}
        </Text>
        <Group grow align="flex-start">
          <TextInput
            label="Name"
            description="Names the LVM volume group"
            value={name}
            onChange={(e) => setName(e.currentTarget.value)}
            error={nameErr ?? (taken ? 'This node already has a disk with that name' : null)}
            size="xs"
          />
          <NumberInput
            label="Size"
            value={sizeGB}
            onChange={(v) => setSizeGB(Number(v) || 0)}
            min={MIN_DISK_GB}
            max={MAX_DISK_GB}
            step={10}
            suffix=" GB"
            size="xs"
          />
        </Group>
        <Group grow align="flex-start">
          {!pool && (
            <TextInput
              label="Mount path"
              value={mountPath}
              onChange={(e) => setMountPath(e.currentTarget.value)}
              error={mountErr}
              size="xs"
            />
          )}
          <Select
            label="Filesystem"
            data={DISK_FS_TYPES as unknown as string[]}
            value={fsType}
            onChange={(v) => setFsType(v ?? 'ext4')}
            allowDeselect={false}
            size="xs"
          />
        </Group>
        <Group justify="flex-end" gap="xs">
          <Button size="xs" variant="subtle" onClick={onDone}>
            Cancel
          </Button>
          <Button
            size="xs"
            leftSection={<IconPlus size={14} />}
            loading={add.isPending}
            disabled={!ready}
            onClick={() =>
              add.mutate(
                {
                  vm_name: node.vm_name,
                  name,
                  size_gb: sizeGB,
                  mount_path: path,
                  fs_type: fsType as 'ext4' | 'xfs',
                },
                { onSuccess: onDone },
              )
            }
          >
            Add disk
          </Button>
        </Group>
      </Stack>
    </Paper>
  );
}

function DiskRow({
  cluster,
  disk,
  canManage,
}: {
  cluster: Cluster;
  disk: NodeDisk;
  canManage: boolean;
}) {
  const remove = useRemoveNodeDisk(cluster.id);
  const removing = disk.phase === 'removing';
  const pool = feedsStoragePool(disk);
  // The platform's own per-worker disk is derived from the cluster's storage size, so the API
  // refuses to delete it on its own - offering the button would only produce an error.
  const platform = isPlatformStorageDisk(disk);

  // Removing a disk destroys its data, and nothing brings it back - so this confirms, and says so
  // plainly rather than asking "are you sure?".
  const confirmRemove = () =>
    modals.openConfirmModal({
      title: `Remove disk ${disk.name}?`,
      centered: true,
      children: (
        <Stack gap="xs">
          <Text size="sm">
            <b>Everything on {disk.mount_path} is destroyed.</b> The disk is unmounted on{' '}
            <Code>{disk.vm_name}</Code>, its volume group is torn down and the volume is deleted.
            This cannot be undone.
          </Text>
          {pool ? (
            <Text size="sm" c="dimmed">
              Longhorn moves this disk's volume replicas to the rest of the pool first. Any volume
              with nowhere else to go stays degraded until there is room for it again.
            </Text>
          ) : (
            <Text size="sm" c="dimmed">
              Anything still writing to {disk.mount_path} will start seeing errors.
            </Text>
          )}
        </Stack>
      ),
      labels: { confirm: 'Remove disk', cancel: 'Cancel' },
      confirmProps: { color: 'red' },
      onConfirm: () => remove.mutate({ vmName: disk.vm_name, disk: disk.name }),
    });

  return (
    <Table.Tr>
      <Table.Td>
        <Group gap={6} wrap="nowrap">
          <Text size="sm" ff="monospace">
            {disk.name}
          </Text>
          {pool && (
            <Tooltip
              label={
                platform
                  ? "This worker's storage disk, provisioned with the cluster"
                  : 'Extra capacity in the cluster storage pool'
              }
            >
              <Badge size="xs" variant="light" color="grape">
                {platform ? 'storage' : 'pool'}
              </Badge>
            </Tooltip>
          )}
        </Group>
      </Table.Td>
      <Table.Td>
        <Text size="sm">{disk.size_gb} GB</Text>
      </Table.Td>
      <Table.Td>
        <Code>{disk.mount_path}</Code>
      </Table.Td>
      <Table.Td>
        <Text size="xs" c="dimmed">
          {disk.fs_type}
        </Text>
      </Table.Td>
      <Table.Td>
        <Badge size="sm" variant="dot" color={diskPhaseColor(disk.phase)}>
          {disk.phase}
        </Badge>
      </Table.Td>
      <Table.Td>
        <Group gap={4} justify="flex-end" wrap="nowrap">
          {canManage && (
            <Tooltip
              label={
                platform
                  ? "The cluster's own storage disk - sized at creation, removed only with its node"
                  : removing
                    ? 'Already being removed'
                    : 'Remove disk (destroys its data)'
              }
            >
              <ActionIcon
                variant="subtle"
                color="red"
                onClick={confirmRemove}
                disabled={removing || platform}
                loading={remove.isPending}
              >
                <IconTrash size={15} />
              </ActionIcon>
            </Tooltip>
          )}
        </Group>
      </Table.Td>
    </Table.Tr>
  );
}

export function NodeDetailPane({
  cluster,
  node,
  onClose,
  onSsh,
}: {
  cluster: Cluster;
  node: Node | null;
  onClose: () => void;
  // Opens an SSH session to this node. Present only for write-access actors (ClusterDetail passes it
  // when canManage); undefined leaves the Access section read-only. The API is the authoritative gate.
  onSsh?: (n: Node) => void;
}) {
  const [adding, setAdding] = useState(false);
  if (!node) return null;

  const isCP = node.role === 'control-plane';
  const disks = (cluster.node_disks ?? [])
    .filter((d) => d.vm_name === node.vm_name)
    .sort((a, b) => a.name.localeCompare(b.name));
  // Extra disks are a worker feature: a control plane's storage is the platform's business (etcd
  // lives there), and the API rejects a disk on one regardless.
  const canHaveDisks = !isCP;
  const canManage = cluster.can_manage && cluster.phase === 'Ready';
  // SSH does NOT gate on phase Ready - it needs only a booted VM with an IP, and a half-provisioned
  // node is exactly when getting onto the box is useful (see the API handler). It only needs write
  // access, the node's IP, and a wired handler.
  const canSsh = !!onSsh && cluster.can_manage && !!node.ip;

  return (
    <Drawer
      opened={!!node}
      onClose={() => {
        setAdding(false);
        onClose();
      }}
      position="right"
      size="lg"
      title={
        <Group gap={8}>
          {isCP ? <IconServerBolt size={18} /> : <IconServer size={18} />}
          <Text fw={700} ff="monospace">
            {node.vm_name}
          </Text>
          <Badge size="xs" variant="outline" color={isCP ? 'grape' : 'gray'} radius="sm">
            {node.role}
          </Badge>
        </Group>
      }
    >
      <Stack gap="lg">
        <Stack gap={6}>
          <Group gap="xs">
            <Text size="sm" c="dimmed" w={80}>
              Status
            </Text>
            <Badge size="sm" variant="dot" color={nodeColor(node.phase)}>
              {node.phase || 'pending'}
            </Badge>
          </Group>
          <Group gap="xs">
            <Text size="sm" c="dimmed" w={80}>
              IP
            </Text>
            {node.ip ? <Code>{node.ip}</Code> : <Text c="dimmed">-</Text>}
          </Group>
          <Group gap="xs">
            <Text size="sm" c="dimmed" w={80}>
              Pool
            </Text>
            {node.pool ? (
              <Badge size="xs" variant="light" color="brand" radius="sm">
                {node.pool}
              </Badge>
            ) : (
              <Text size="sm" c="dimmed">
                - (control planes belong to no pool)
              </Text>
            )}
          </Group>
          {node.image && (
            <Group gap="xs" wrap="nowrap" align="flex-start">
              <Text size="sm" c="dimmed" w={80}>
                Image
              </Text>
              <Text size="xs" ff="monospace" style={{ wordBreak: 'break-all' }}>
                {node.image}
              </Text>
            </Group>
          )}
        </Stack>

        {onSsh && (
          <>
            <Divider />
            <Stack gap="sm">
              <Group gap={8}>
                <ThemeIcon variant="light" size="sm" radius="sm">
                  <IconTerminal2 size={14} />
                </ThemeIcon>
                <Text fw={600}>Access</Text>
              </Group>
              <Text size="xs" c="dimmed">
                Open a terminal on this node as the <b>kaas</b> user (passwordless sudo) - for
                inspecting the OS, systemd units and logs. The session is audited on the Activity tab.
              </Text>
              <Button
                variant="light"
                leftSection={<IconTerminal2 size={16} />}
                disabled={!canSsh}
                onClick={() => onSsh(node)}
                style={{ alignSelf: 'flex-start' }}
              >
                SSH to node
              </Button>
              {!node.ip && (
                <Text size="xs" c="dimmed">
                  This node has no IP yet - it is still being provisioned. SSH opens once it has an
                  address.
                </Text>
              )}
            </Stack>
          </>
        )}

        <Divider />

        <Stack gap="sm">
          <Group justify="space-between" align="center">
            <Group gap={8}>
              <ThemeIcon variant="light" size="sm" radius="sm">
                <IconDeviceSdCard size={14} />
              </ThemeIcon>
              <Text fw={600}>Disks</Text>
            </Group>
            {canHaveDisks && canManage && !adding && (
              <Button
                size="xs"
                variant="light"
                leftSection={<IconPlus size={14} />}
                onClick={() => setAdding(true)}
              >
                Add disk
              </Button>
            )}
          </Group>

          {!canHaveDisks ? (
            <Alert
              variant="light"
              color="gray"
              icon={<IconInfoCircle size={16} />}
              title="Control-plane node"
            >
              Extra disks are for worker nodes. A control plane&apos;s storage holds etcd and is
              managed by the platform.
            </Alert>
          ) : (
            <>
              {/* The root disk isn't an extra disk and can't be managed here, but leaving it out
                  entirely would make the section read as "this node has no storage". */}
              <Text size="xs" c="dimmed">
                The root disk is sized by the node&apos;s pool and is not listed here. Extra disks
                are formatted with LVM and mounted at the path you choose.
              </Text>

              {adding && (
                <AddDiskForm cluster={cluster} node={node} onDone={() => setAdding(false)} />
              )}

              {disks.length === 0 ? (
                <Text size="sm" c="dimmed" py="xs">
                  No extra disks on this node.
                </Text>
              ) : (
                <Table.ScrollContainer minWidth={480}>
                  <Table verticalSpacing="xs" highlightOnHover>
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>Name</Table.Th>
                        <Table.Th>Size</Table.Th>
                        <Table.Th>Mount</Table.Th>
                        <Table.Th>FS</Table.Th>
                        <Table.Th>Status</Table.Th>
                        <Table.Th />
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {disks.map((d) => (
                        <DiskRow key={d.name} cluster={cluster} disk={d} canManage={canManage} />
                      ))}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              )}

              {canHaveDisks && cluster.can_manage && cluster.phase !== 'Ready' && (
                <Text size="xs" c="dimmed">
                  Disks can be added once the cluster is Ready (it is {cluster.phase}).
                </Text>
              )}
            </>
          )}
        </Stack>
      </Stack>
    </Drawer>
  );
}
