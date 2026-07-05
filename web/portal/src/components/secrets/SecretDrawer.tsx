// The per-Secret detail drawer: Keys, Details and YAML. Values are NEVER shown - the server redacts
// them, so this renders each key with its byte length and a "••••••" placeholder, and the YAML has its
// data values scrubbed. A Secret synced from Vault by the External Secrets Operator is badged with the
// ExternalSecret that owns it.

import { useState } from 'react';
import { Drawer, Group, Text, Badge, Stack, Tabs, Table, Tooltip } from '@mantine/core';
import { IconLock } from '@tabler/icons-react';
import type { SecretSummary } from '../../lib/types';
import { useSecret, useSecretManifest } from '../../lib/queries';
import { relative } from '../../lib/format';
import { YamlView } from '../YamlView';
import { DetailRow, LabelBadges } from '../storage/shared';

export function SecretDrawer({
  clusterId,
  secret,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  secret: SecretSummary | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<string | null>('keys');
  const ns = secret?.namespace ?? '';
  const name = secret?.name ?? '';

  const { data: detail } = useSecret(clusterId, ns, name, opened && !!secret);
  const { data: yaml, isLoading, error } = useSecretManifest(
    clusterId,
    ns,
    name,
    opened && !!secret && tab === 'yaml',
  );

  return (
    <Drawer
      opened={opened}
      onClose={onClose}
      position="right"
      size="xl"
      title={
        <Group gap="sm" wrap="nowrap">
          <IconLock size={18} />
          <Text fw={700} style={{ wordBreak: 'break-all' }}>
            {name}
          </Text>
          {secret?.managed_by && (
            <Tooltip label={`Synced from Vault by ExternalSecret ${secret.managed_by}`}>
              <Badge color="violet" variant="light" size="sm">
                Vault
              </Badge>
            </Tooltip>
          )}
        </Group>
      }
    >
      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="keys">Keys</Tabs.Tab>
          <Tabs.Tab value="details">Details</Tabs.Tab>
          <Tabs.Tab value="yaml">YAML</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="keys">
          <Text size="xs" c="dimmed" mb="sm">
            Secret values are redacted. Only key names and sizes are shown.
          </Text>
          <Table verticalSpacing="sm" highlightOnHover withTableBorder>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Key</Table.Th>
                <Table.Th>Value</Table.Th>
                <Table.Th style={{ textAlign: 'right' }}>Size</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {(detail?.key_info ?? []).map((k) => (
                <Table.Tr key={k.key}>
                  <Table.Td>
                    <Text size="sm" ff="monospace" fw={600}>
                      {k.key}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed" ff="monospace">
                      ••••••••
                    </Text>
                  </Table.Td>
                  <Table.Td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                    <Text size="sm" c="dimmed">
                      {k.bytes} B
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
              {detail && (detail.key_info ?? []).length === 0 && (
                <Table.Tr>
                  <Table.Td colSpan={3}>
                    <Text c="dimmed" size="sm">
                      This Secret has no data.
                    </Text>
                  </Table.Td>
                </Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Tabs.Panel>

        <Tabs.Panel value="details">
          {secret && (
            <Stack gap="lg">
              <Stack gap={6}>
                <DetailRow label="Namespace" value={secret.namespace} />
                <DetailRow label="Type" value={secret.type} mono />
                <DetailRow label="Keys" value={String(secret.data_count)} />
                {secret.managed_by && (
                  <DetailRow
                    label="Synced from"
                    value={
                      <Badge color="violet" variant="light" size="sm">
                        Vault → {secret.managed_by}
                      </Badge>
                    }
                  />
                )}
                <DetailRow label="Immutable" value={secret.immutable ? 'Yes' : 'No'} />
                <DetailRow label="Created" value={relative(secret.created_at)} />
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
