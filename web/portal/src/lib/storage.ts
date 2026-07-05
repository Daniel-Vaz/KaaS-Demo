// Presentation helpers for the Storage page: claim-phase colouring, access-mode wording, and
// capacity parsing/summing (so the page can total what a namespace has provisioned).

import type { PVCSummary } from './types';

// pvcStatusColor maps a claim's binding phase to a Mantine colour. Pending is deliberately yellow
// rather than red: a WaitForFirstConsumer claim is unbound on purpose and perfectly healthy, while a
// Lost claim has lost its backing volume and really is broken.
export function pvcStatusColor(status: string): string {
  switch (status) {
    case 'Bound':
      return 'green';
    case 'Pending':
      return 'yellow';
    case 'Lost':
      return 'red';
    default:
      return 'gray';
  }
}

// ACCESS_MODE_LABELS spells out the abbreviated access modes the API returns, for tooltips - the
// table shows the short form (RWO), which is what operators read, with the full meaning on hover.
export const ACCESS_MODE_LABELS: Record<string, string> = {
  RWO: 'ReadWriteOnce - mountable read-write by a single node',
  ROX: 'ReadOnlyMany - mountable read-only by many nodes',
  RWX: 'ReadWriteMany - mountable read-write by many nodes',
  RWOP: 'ReadWriteOncePod - mountable read-write by a single pod',
};

export function accessModeLabel(mode: string): string {
  return ACCESS_MODE_LABELS[mode] ?? mode;
}

// BINARY_UNITS are Kubernetes quantity suffixes, in ascending order of magnitude. Kubernetes uses
// binary (Ki/Mi/Gi) and decimal (K/M/G) suffixes; both appear in real manifests.
const BINARY_SUFFIXES: Record<string, number> = {
  Ki: 1024,
  Mi: 1024 ** 2,
  Gi: 1024 ** 3,
  Ti: 1024 ** 4,
  Pi: 1024 ** 5,
  Ei: 1024 ** 6,
  K: 1000,
  M: 1000 ** 2,
  G: 1000 ** 3,
  T: 1000 ** 4,
  P: 1000 ** 5,
  E: 1000 ** 6,
};

// parseQuantity converts a Kubernetes storage quantity ("8Gi", "500M", "1024") to bytes, returning 0
// for anything it can't read - the callers only sum and format, so a bad value should read as "no
// contribution" rather than blow up the page.
export function parseQuantity(q: string | undefined): number {
  if (!q) return 0;
  const m = /^(\d+(?:\.\d+)?)\s*([A-Za-z]*)$/.exec(q.trim());
  if (!m) return 0;
  const value = parseFloat(m[1]);
  if (Number.isNaN(value)) return 0;
  const suffix = m[2];
  if (!suffix) return value;
  const mult = BINARY_SUFFIXES[suffix];
  return mult ? value * mult : 0;
}

// formatBytes renders a byte count back as a binary quantity ("8 GiB"), for totals that are summed
// across claims and so have no single source suffix to preserve.
export function formatBytes(bytes: number): string {
  if (bytes <= 0) return '0';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let v = bytes;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  // One decimal only when it adds information (8.5 GiB, but 8 GiB not 8.0 GiB).
  const rounded = v >= 10 || Number.isInteger(v) ? Math.round(v) : Math.round(v * 10) / 10;
  return `${rounded} ${units[i]}`;
}

// totalCapacity sums the effective capacity across claims - what this cluster/namespace actually has
// provisioned, the headline an operator wants above the table.
export function totalCapacity(pvcs: PVCSummary[]): string {
  return formatBytes(pvcs.reduce((sum, p) => sum + parseQuantity(p.capacity || p.requested), 0));
}
