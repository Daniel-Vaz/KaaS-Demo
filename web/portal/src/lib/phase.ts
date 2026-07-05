// One shared mapping from a cluster Phase to its colour, label, and meaning - used by badges,
// the phase stepper, the dashboard donut, and status dots so the whole portal reads the same way.

import type { Phase } from './types';

export interface PhaseMeta {
  label: string;
  color: string; // a Mantine colour name
  /** true while the reconciler is actively converging (spinner / progress affordances). */
  active: boolean;
}

export const PHASE_META: Record<Phase, PhaseMeta> = {
  Pending: { label: 'Pending', color: 'gray', active: true },
  ProvisioningInfra: { label: 'Provisioning infra', color: 'violet', active: true },
  InfraReady: { label: 'Infra ready', color: 'violet', active: true },
  ControlPlaneReady: { label: 'Control plane ready', color: 'grape', active: true },
  WorkersReady: { label: 'Workers ready', color: 'indigo', active: true },
  InstallingAddons: { label: 'Installing add-ons', color: 'blue', active: true },
  Ready: { label: 'Ready', color: 'teal', active: false },
  Updating: { label: 'Updating', color: 'blue', active: true },
  Upgrading: { label: 'Upgrading', color: 'cyan', active: true },
  // Periodic maintenance on a Ready cluster: neither is a user-requested change, and both return to
  // Ready by themselves - but they ARE the reconciler converging, so they read as active.
  RenewingCerts: { label: 'Renewing certificates', color: 'cyan', active: true },
  DefragmentingEtcd: { label: 'Defragmenting etcd', color: 'cyan', active: true },
  SnapshottingEtcd: { label: 'Backing up', color: 'cyan', active: true },
  // Repair is the one non-user-requested transition that is not routine maintenance: the cluster is
  // degraded and the platform is acting on it. Orange rather than cyan so it does not read as
  // housekeeping in a list of clusters.
  Repairing: { label: 'Repairing', color: 'orange', active: true },
  Deleting: { label: 'Deleting', color: 'orange', active: true },
  Deleted: { label: 'Deleted', color: 'gray', active: false },
  Failed: { label: 'Failed', color: 'red', active: false },
};

export function phaseMeta(phase: Phase | string): PhaseMeta {
  return PHASE_META[phase as Phase] ?? { label: String(phase), color: 'gray', active: false };
}

// The happy-path order the reconciler advances a cluster through, for the detail stepper.
export const PROVISION_STEPS: Phase[] = [
  'Pending',
  'ProvisioningInfra',
  'InfraReady',
  'ControlPlaneReady',
  'WorkersReady',
  'InstallingAddons',
  'Ready',
];

// Colour for an add-on's phase chip.
export function addonColor(phase: string): string {
  switch (phase) {
    case 'ready':
      return 'teal';
    case 'removing':
      return 'orange';
    case 'failed':
      return 'red';
    default:
      return 'violet'; // pending / installing
  }
}

// Colour for a node's phase chip.
export function nodeColor(phase: string): string {
  const p = phase.toLowerCase();
  if (p === 'ready') return 'teal';
  if (p.includes('fail') || p.includes('error')) return 'red';
  if (p === '' || p === 'pending') return 'gray';
  return 'violet';
}
