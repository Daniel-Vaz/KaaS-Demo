// The per-ConfigMap detail drawer: Details, Data and YAML. A ConfigMap is not secret, so its values
// are shown in full (mirrors the server, which returns them). Binary entries list their keys only.

import { useState } from 'react';
import { Drawer, Group, Text, Badge, Stack, Tabs, Code, ScrollArea } from '@mantine/core';
import type { ConfigMapSummary } from '../../lib/types';
import { useConfigMap, useConfigMapManifest } from '../../lib/queries';
import { relative } from '../../lib/format';
import { YamlView } from '../YamlView';
import { DetailRow, LabelBadges } from '../storage/shared';

export function ConfigMapDrawer({
  clusterId,
  configMap,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  configMap: ConfigMapSummary | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<string | null>('data');
  const ns = configMap?.namespace ?? '';
  const name = configMap?.name ?? '';

  const { data: detail } = useConfigMap(clusterId, ns, name, opened && !!configMap);
  const { data: yaml, isLoading, error } = useConfigMapManifest(
    clusterId,
    ns,
    name,
    opened && !!configMap && tab === 'yaml',
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
          {configMap?.immutable && (
            <Badge color="gray" variant="light" size="sm">
              immutable
            </Badge>
          )}
        </Group>
      }
    >
      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="data">Data</Tabs.Tab>
          <Tabs.Tab value="details">Details</Tabs.Tab>
          <Tabs.Tab value="yaml">YAML</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="data">
          <Stack gap="md">
            {Object.entries(detail?.data ?? {}).map(([k, v]) => (
              <div key={k}>
                <Text size="sm" fw={600} ff="monospace" mb={4}>
                  {k}
                </Text>
                <ScrollArea.Autosize mah={220}>
                  <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                    {v}
                  </Code>
                </ScrollArea.Autosize>
              </div>
            ))}
            {(detail?.binary_keys ?? []).map((k) => (
              <DetailRow key={k} label={k} value={<Badge variant="light" color="gray" size="sm">binary</Badge>} mono />
            ))}
            {detail && Object.keys(detail.data ?? {}).length === 0 && (detail.binary_keys ?? []).length === 0 && (
              <Text c="dimmed" size="sm">
                This ConfigMap has no data.
              </Text>
            )}
          </Stack>
        </Tabs.Panel>

        <Tabs.Panel value="details">
          {configMap && (
            <Stack gap="lg">
              <Stack gap={6}>
                <DetailRow label="Namespace" value={configMap.namespace} />
                <DetailRow label="Keys" value={String(configMap.data_count)} />
                <DetailRow label="Immutable" value={configMap.immutable ? 'Yes' : 'No'} />
                <DetailRow label="Created" value={relative(configMap.created_at)} />
              </Stack>
              <LabelBadges title="Labels" values={detail?.labels} />
              <LabelBadges title="Annotations" values={detail?.annotations} />
            </Stack>
          )}
        </Tabs.Panel>

        <Tabs.Panel value="yaml">
          <YamlView yaml={yaml} filename={name} isLoading={isLoading} error={error} />
        </Tabs.Panel>
      </Tabs>
    </Drawer>
  );
}
