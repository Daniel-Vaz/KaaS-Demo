import { Badge } from '@mantine/core';
import { IconCloud, IconServer2, IconServerBolt } from '@tabler/icons-react';
import { clusterProvider, providerLabel } from '../lib/cluster';
import type { Cluster } from '../lib/types';

// ProviderBadge names the infrastructure a cluster runs on. Once a deployment offers more than one
// provider, "where does this cluster actually live" stops being a detail and becomes the first
// thing you need to know about a row - the same cluster name means a VM on the laptop's hypervisor,
// a VM in vCenter, or a VM on Proxmox, and nothing else on the row tells you which.
const PROVIDER_STYLE: Record<string, { color: string; Icon: typeof IconCloud }> = {
  vsphere: { color: 'indigo', Icon: IconCloud },
  proxmox: { color: 'orange', Icon: IconServer2 },
  kvm: { color: 'teal', Icon: IconServerBolt },
};
export function ProviderBadge({ cluster, size = 'sm' }: { cluster: Cluster; size?: string }) {
  const provider = clusterProvider(cluster);
  const { color, Icon } = PROVIDER_STYLE[provider] ?? PROVIDER_STYLE.kvm;
  return (
    <Badge color={color} variant="light" size={size} leftSection={<Icon size={12} />}>
      {providerLabel(provider)}
    </Badge>
  );
}
