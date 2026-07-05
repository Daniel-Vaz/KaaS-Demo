// The per-class detail drawer: Details and YAML. A StorageClass has no status and no events of its
// own - it is a template the provisioner reads - so unlike the claim drawer there is no Events tab,
// and the whole Details tab comes from the list row (only the YAML is fetched on open).

import { useState } from 'react';
import { Drawer, Group, Text, Badge, Stack, Tabs } from '@mantine/core';
import type { StorageClass } from '../../lib/types';
import { useStorageClassManifest } from '../../lib/queries';
import { relative } from '../../lib/format';
import { YamlView } from '../YamlView';
import { LabelBadges, DetailRow } from './shared';

export function StorageClassDrawer({
  clusterId,
  storageClass,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  storageClass: StorageClass | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<string | null>('details');
  const name = storageClass?.name ?? '';

  const { data: yaml, isLoading, error } = useStorageClassManifest(
    clusterId,
    name,
    opened && !!storageClass && tab === 'yaml',
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
            {name}
          </Text>
          {storageClass?.is_default && (
            <Badge color="blue" variant="light" size="sm">
              default
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

        <Tabs.Panel value="details">
          {storageClass && <ClassDetails sc={storageClass} />}
        </Tabs.Panel>

        <Tabs.Panel value="yaml">
          <YamlView yaml={yaml} filename={name} isLoading={isLoading} error={error} />
        </Tabs.Panel>
      </Tabs>
    </Drawer>
  );
}

function ClassDetails({ sc }: { sc: StorageClass }) {
  return (
    <Stack gap="lg">
      <Stack gap={6}>
        <DetailRow label="Provisioner" value={sc.provisioner} mono />
        <DetailRow label="Reclaim policy" value={sc.reclaim_policy || '-'} />
        <DetailRow label="Volume binding mode" value={sc.volume_binding_mode || '-'} />
        <DetailRow
          label="Volume expansion"
          value={
            <Badge color={sc.allow_expansion ? 'green' : 'gray'} variant="light" size="sm">
              {sc.allow_expansion ? 'Allowed' : 'Not allowed'}
            </Badge>
          }
        />
        <DetailRow
          label="Default class"
          value={
            <Badge color={sc.is_default ? 'blue' : 'gray'} variant="light" size="sm">
              {sc.is_default ? 'Yes' : 'No'}
            </Badge>
          }
        />
        <DetailRow label="Created" value={relative(sc.created_at)} />
      </Stack>

      {/* Parameters are the provisioner's own vocabulary (disk type, fs type, replication…), so they
          are shown verbatim rather than interpreted. */}
      <LabelBadges title="Parameters" values={sc.parameters} />
      {sc.mount_options && sc.mount_options.length > 0 && (
        <section>
          <Text fw={600} mb="xs">
            Mount options
          </Text>
          <Group gap="xs">
            {sc.mount_options.map((o) => (
              <Badge key={o} variant="outline" color="gray" size="sm" radius="sm" style={{ textTransform: 'none' }}>
                {o}
              </Badge>
            ))}
          </Group>
        </section>
      )}
      <LabelBadges title="Labels" values={sc.labels} />
    </Stack>
  );
}
