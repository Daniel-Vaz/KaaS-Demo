import {
  IconChartAreaLine,
  IconGauge,
  IconShieldHalf,
  IconRouter,
  IconRoute,
  IconRefreshDot,
  IconGitBranch,
  IconGavel,
  IconCoin,
  IconBolt,
  IconKey,
  IconShieldLock,
  IconCertificate,
  IconWorld,
  IconCube,
  IconDatabase,
} from '@tabler/icons-react';
import type { Icon } from '@tabler/icons-react';

export interface AddonMeta {
  icon: Icon;
  color: string;
  category: string;
}

// Per-add-on presentation: an evocative icon, an accent color, and a coarse category used to
// group the picker. Keyed by the catalog add-on name; unknown add-ons (e.g. custom-catalog ones)
// fall back to a neutral cube. Purely cosmetic - nothing here drives provisioning.
const META: Record<string, AddonMeta> = {
  'kube-prometheus-stack': { icon: IconChartAreaLine, color: 'orange', category: 'Observability' },
  'metrics-server': { icon: IconGauge, color: 'teal', category: 'Observability' },
  opencost: { icon: IconCoin, color: 'yellow', category: 'Observability' },
  kepler: { icon: IconBolt, color: 'green', category: 'Observability' },
  'trivy-operator': { icon: IconShieldHalf, color: 'red', category: 'Security & Policy' },
  kyverno: { icon: IconGavel, color: 'lime', category: 'Security & Policy' },
  'cert-manager': { icon: IconCertificate, color: 'teal', category: 'Security & Policy' },
  'external-secrets': { icon: IconKey, color: 'pink', category: 'Security & Policy' },
  keycloak: { icon: IconShieldLock, color: 'violet', category: 'Security & Policy' },
  metallb: { icon: IconRouter, color: 'grape', category: 'Networking & Ingress' },
  'envoy-gateway': { icon: IconRoute, color: 'indigo', category: 'Networking & Ingress' },
  'external-dns': { icon: IconWorld, color: 'blue', category: 'Networking & Ingress' },
  argocd: { icon: IconRefreshDot, color: 'cyan', category: 'Delivery' },
  flux: { icon: IconGitBranch, color: 'blue', category: 'Delivery' },
  longhorn: { icon: IconDatabase, color: 'cyan', category: 'Storage' },
  openebs: { icon: IconDatabase, color: 'indigo', category: 'Storage' },
};

const FALLBACK: AddonMeta = { icon: IconCube, color: 'gray', category: 'Other' };

export function addonMeta(name: string): AddonMeta {
  return META[name] ?? FALLBACK;
}

// Order categories are presented in the picker. Categories not listed sort last, alphabetically.
export const CATEGORY_ORDER = [
  'Observability',
  'Security & Policy',
  'Networking & Ingress',
  'Delivery',
  'Storage',
  'Other',
];
