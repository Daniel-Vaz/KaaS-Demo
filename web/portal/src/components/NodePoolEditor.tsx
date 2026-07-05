import { Stack, Group, Card, Text, TextInput, NumberInput, Select, ActionIcon, Button } from '@mantine/core';
import { IconTrash, IconPlus, IconServer2 } from '@tabler/icons-react';
import { SIZES, MAX_ROOT_DISK_GB } from '../lib/types';
import type { NodePool } from '../lib/types';

// NodePoolEditor edits a cluster's desired worker topology as a whole list - the same shape the API
// takes (see UpdateClusterRequest.node_pools). It is used both by the create wizard and by the
// cluster-detail panel, so the two can't drift on validation or on what a pool even looks like.
//
// `locked` names the pools that already exist on a live cluster: their name, size and root-disk size
// are all immutable server-side (none can change without rolling every node in the pool), so those
// inputs are disabled and only the worker count is editable. On the create wizard nothing is locked.
export function NodePoolEditor({
  pools,
  onChange,
  locked = [],
  controlPlaneSize,
}: {
  pools: NodePool[];
  onChange: (pools: NodePool[]) => void;
  locked?: string[];
  controlPlaneSize?: string;
}) {
  const sizeOptions = Object.keys(SIZES).map((s) => ({ value: s, label: sizeLabel(s) }));

  const set = (i: number, patch: Partial<NodePool>) =>
    onChange(pools.map((p, j) => (j === i ? { ...p, ...patch } : p)));

  const add = () =>
    onChange([
      ...pools,
      { name: nextPoolName(pools), size: controlPlaneSize ?? 'small', desired_workers: 1 },
    ]);

  return (
    <Stack gap="sm">
      {pools.map((p, i) => {
        const isLocked = locked.includes(p.name);
        return (
          <Card key={i} withBorder radius="md" padding="sm">
            <Group align="flex-end" wrap="nowrap" gap="sm">
              <TextInput
                label="Pool name"
                description={isLocked ? 'Fixed once created' : 'Used in node names and the pool label'}
                value={p.name}
                disabled={isLocked}
                onChange={(e) => set(i, { name: e.currentTarget.value })}
                error={poolNameError(p.name, pools, i)}
                style={{ flex: 1 }}
              />
              <Select
                label="Size"
                description={isLocked ? 'Immutable' : undefined}
                data={sizeOptions}
                value={p.size}
                disabled={isLocked}
                allowDeselect={false}
                onChange={(v) => v && set(i, { size: v })}
                w={190}
              />
              {/* The workers' ROOT disk. Blank = the size's default (50 GB); it can only grow that,
                  because a node's volume is a copy-on-write clone of the golden image. Immutable
                  once the pool exists, like the size - to add storage to a running node, attach an
                  extra disk from the node's detail pane instead. */}
              <NumberInput
                label="Disk"
                description={isLocked ? 'Immutable' : undefined}
                placeholder={String(SIZES[p.size]?.diskGB ?? 50)}
                min={SIZES[p.size]?.diskGB ?? 50}
                max={MAX_ROOT_DISK_GB}
                step={10}
                suffix=" GB"
                value={p.disk_gb ?? ''}
                disabled={isLocked}
                onChange={(v) => set(i, { disk_gb: typeof v === 'number' && v > 0 ? v : undefined })}
                error={poolDiskError(p)}
                w={130}
              />
              <NumberInput
                label="Workers"
                min={0}
                max={20}
                value={p.desired_workers}
                onChange={(v) => set(i, { desired_workers: typeof v === 'number' ? v : 0 })}
                w={110}
              />
              <ActionIcon
                variant="subtle"
                color="red"
                size="lg"
                mb={4}
                aria-label={`Remove pool ${p.name}`}
                onClick={() => onChange(pools.filter((_, j) => j !== i))}
              >
                <IconTrash size={16} />
              </ActionIcon>
            </Group>
          </Card>
        );
      })}

      {pools.length === 0 && (
        <Card withBorder radius="md" padding="lg">
          <Group gap="xs" justify="center" c="dimmed">
            <IconServer2 size={16} />
            <Text size="sm">No node pools - this cluster will run its control plane only.</Text>
          </Group>
        </Card>
      )}

      <Group>
        <Button variant="light" leftSection={<IconPlus size={16} />} onClick={add}>
          Add node pool
        </Button>
      </Group>
    </Stack>
  );
}

function sizeLabel(s: string): string {
  const spec = SIZES[s];
  return `${s} - ${spec.cpus} vCPU · ${Math.round(spec.memMB / 1024)} GB`;
}

// nextPoolName suggests a free name ("pool-2", "pool-3", ...) so adding a pool never starts out
// invalid with a duplicate.
function nextPoolName(pools: NodePool[]): string {
  const taken = new Set(pools.map((p) => p.name));
  for (let i = 2; ; i++) {
    const name = `pool-${i}`;
    if (!taken.has(name)) return name;
  }
}

// poolNameError mirrors domain.ValidatePoolName / ValidateNodePools closely enough to catch typos
// before submit. The server is the authoritative gate (it also knows the cluster name, which bounds
// the total hostname length).
const POOL_NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

export function poolNameError(name: string, pools: NodePool[], index: number): string | null {
  if (!name) return 'Required';
  if (!POOL_NAME_RE.test(name)) return 'Lowercase letters, digits and dashes only';
  if (name === 'cp') return 'Reserved for control-plane nodes';
  if (pools.some((p, j) => j !== index && p.name === name)) return 'Duplicate pool name';
  return null;
}

// poolDiskError mirrors the root-disk bounds in domain.ValidateNodePools: the override may only GROW
// the size's default (a node's volume is a COW clone of the golden image, and libvirt/vSphere both
// refuse a volume smaller than the image it clones), and is capped as a typo guard.
export function poolDiskError(p: NodePool): string | null {
  if (!p.disk_gb) return null; // unset = the size's default
  const floor = SIZES[p.size]?.diskGB ?? 50;
  if (p.disk_gb < floor) return `At least ${floor} GB (the ${p.size} default)`;
  if (p.disk_gb > MAX_ROOT_DISK_GB) return `At most ${MAX_ROOT_DISK_GB} GB`;
  return null;
}

// poolsValid reports whether the whole list is submittable.
export function poolsValid(pools: NodePool[]): boolean {
  return pools.every((p, i) => poolNameError(p.name, pools, i) === null && poolDiskError(p) === null);
}
