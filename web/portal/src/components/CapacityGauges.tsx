import { Stack, Text, SimpleGrid, Group, Badge } from '@mantine/core';
import { IconCpu, IconDatabase, IconDeviceSdCard } from '@tabler/icons-react';
import { gib } from '../lib/format';
import { providerLabel } from '../lib/cluster';
import { Gauge } from './Gauge';
import type { CapacityReport, ProviderQuota } from '../lib/types';

// ProviderGauges is one infrastructure's pair of gauges. Quota is granted and charged per
// infrastructure - a spare core on the KVM host can't run a vSphere VM - so this, not the summed
// total, is the number that tells a tenant whether their next cluster will be admitted.
export function ProviderGauges({ q }: { q: ProviderQuota }) {
  return (
    <div>
      <Group gap={8} mb={6}>
        <Text size="sm" fw={600}>
          {providerLabel(q.provider)}
        </Text>
        {q.total_vcpu === 0 && q.total_mem_mb === 0 && q.total_disk_gb === 0 && (
          <Badge size="xs" variant="light" color="gray" radius="sm">
            no capacity granted
          </Badge>
        )}
      </Group>
      <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="md">
        <Gauge
          icon={IconCpu}
          label="vCPU"
          used={q.used_vcpu}
          total={q.total_vcpu}
          display={`${q.used_vcpu} / ${q.total_vcpu}`}
        />
        <Gauge
          icon={IconDatabase}
          label="Memory"
          used={q.used_mem_mb}
          total={q.total_mem_mb}
          display={`${gib(q.used_mem_mb)} / ${gib(q.total_mem_mb)} GB`}
        />
        {/* Storage: every node's root disk plus every extra disk. It is the dimension a per-pool
            root-disk override or an extra disk actually spends, so a tenant needs to see it to know
            whether their next disk will be admitted. */}
        <Gauge
          icon={IconDeviceSdCard}
          label="Disk"
          used={q.used_disk_gb}
          total={q.total_disk_gb}
          display={`${q.used_disk_gb} / ${q.total_disk_gb} GB`}
        />
      </SimpleGrid>
    </div>
  );
}

// CapacityGauges renders the actor's quota, one section per infrastructure. It deliberately never
// shows a single pooled figure: the sum is not a budget anything can be admitted against.
export function CapacityGauges({ cap }: { cap: CapacityReport }) {
  const providers = cap.providers ?? [];
  // Older payloads (and any deployment that somehow reports no providers) still have the totals.
  if (providers.length === 0) {
    return (
      <ProviderGauges
        q={{
          provider: 'kvm',
          total_vcpu: cap.total_vcpu,
          total_mem_mb: cap.total_mem_mb,
          total_disk_gb: cap.total_disk_gb,
          used_vcpu: cap.used_vcpu,
          used_mem_mb: cap.used_mem_mb,
          used_disk_gb: cap.used_disk_gb,
        }}
      />
    );
  }
  return (
    <Stack gap="lg">
      {providers.map((q) => (
        <ProviderGauges key={q.provider} q={q} />
      ))}
    </Stack>
  );
}
