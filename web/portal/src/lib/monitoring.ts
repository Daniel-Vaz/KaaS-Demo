// Presentation helpers for the Monitoring page: value/axis formatting by unit, a fixed-order
// colorblind-safe categorical palette (from the dataviz design system, validated for both modes),
// reserved status colors, and the per-cluster "monitoring enabled" gate.

import type { Cluster, PanelUnit } from './types';

// MONITORING_ADDON is the catalog add-on that provides this telemetry (mirrors
// internal/monitoring.AddonName).
export const MONITORING_ADDON = 'kube-prometheus-stack';

// monitoringEnabled mirrors internal/monitoring.Enabled: the cluster has the monitoring stack
// installed, so there is a Prometheus to query. The server is the real gate; this just drives the
// page's gating UI without a round-trip.
export function monitoringEnabled(c: Cluster | undefined | null): boolean {
  return !!c?.addons?.some((a) => a.name === MONITORING_ADDON && a.phase === 'installed');
}

// Fixed-order categorical palette - never cycled, never reassigned by rank. Both columns are the same
// eight hues stepped for their surface (see the dataviz skill's validated reference palette: worst
// adjacent CVD ΔE 24.2 light / 10.3 dark). Series ≤ 6 per panel, so we never exhaust it.
const CATEGORICAL_LIGHT = ['#2a78d6', '#1baf7a', '#eda100', '#008300', '#4a3aa7', '#e34948', '#e87ba4', '#eb6834'];
const CATEGORICAL_DARK = ['#3987e5', '#199e70', '#c98500', '#008300', '#9085e9', '#e66767', '#d55181', '#d95926'];

export function seriesColor(index: number, dark: boolean): string {
  const p = dark ? CATEGORICAL_DARK : CATEGORICAL_LIGHT;
  return p[index % p.length];
}

// Reserved status palette (never themed, never reused as a series color). Paired with an icon+label
// wherever it appears, so meaning is never carried by color alone.
export const STATUS_COLOR = {
  good: '#0ca30c',
  warning: '#fab219',
  serious: '#ec835a',
  critical: '#d03b3b',
} as const;

// severityColor maps a Prometheus alert severity to a Mantine color name (badges/icons).
export function severityColor(severity: string): string {
  switch (severity.toLowerCase()) {
    case 'critical':
      return 'red';
    case 'warning':
      return 'orange';
    case 'info':
      return 'blue';
    default:
      return 'gray'; // "none" (e.g. Watchdog)
  }
}

// severityRank orders alerts most-severe first.
export function severityRank(severity: string): number {
  return { critical: 0, warning: 1, info: 2, none: 3 }[severity.toLowerCase()] ?? 4;
}

// gaugeColor: a utilisation ratio's color band - brand until warm, then warning/critical.
export function gaugeColor(ratio: number): string {
  if (ratio >= 0.9) return 'red';
  if (ratio >= 0.75) return 'orange';
  return 'brand';
}

// sloColor: an availability ratio against its target - green above, amber within a hair, red below.
export function sloColor(value: number, target: number): string {
  if (value >= target) return 'teal';
  if (value >= target - 0.005) return 'orange';
  return 'red';
}

// --- value / axis formatting by unit --------------------------------------

// formatValue renders a scalar (stat/gauge/slo value) for its unit.
export function formatValue(v: number, unit: PanelUnit): string {
  switch (unit) {
    case 'ratio':
      return `${(v * 100).toFixed(v >= 0.9995 ? 2 : 1)}%`;
    case 'bytes':
      return formatBytes(v);
    case 'Bps':
      return `${formatBytes(v)}/s`;
    case 'rps':
    case 'ops':
    case 'pps':
      return `${compact(v)}/s`;
    case 's':
      return formatSeconds(v);
    case 'ms':
      return `${compact(v)} ms`;
    case 'cores':
      return v < 0.1 ? `${Math.round(v * 1000)}m` : `${compact(v)} cores`; // millicores below 0.1
    case 'count':
      return formatCount(v);
    default:
      return compact(v);
  }
}

// formatCount renders a count as a whole number - "3", never "3.0" (compact()'s sub-10 decimals are
// for continuous quantities, not things you can count).
function formatCount(v: number): string {
  return Math.abs(v) < 1000 ? Math.round(v).toString() : compact(v);
}

// formatAxis is the compact y-axis tick form for a time-series unit.
export function formatAxis(v: number, unit: PanelUnit): string {
  switch (unit) {
    case 'ratio': {
      // Sub-1% scales (error ratios, disk saturation) need a decimal, or every tick reads "0%".
      const pct = v * 100;
      return `${pct !== 0 && Math.abs(pct) < 1 ? pct.toFixed(1) : Math.round(pct)}%`;
    }
    case 'count':
      return Math.abs(v) < 1000 ? Math.round(v).toString() : compact(v);
    case 'bytes':
      return formatBytes(v);
    case 'Bps':
      return `${formatBytes(v)}/s`;
    case 's':
      return formatSeconds(v);
    case 'cores':
      return v < 0.1 && v > 0 ? `${Math.round(v * 1000)}m` : compact(v);
    default:
      return compact(v);
  }
}

export function unitLabel(unit: PanelUnit): string {
  return { '': '', ratio: '%', count: '', cores: 'cores', rps: 'req/s', ops: 'ops/s', pps: 'pkt/s', bytes: 'bytes', Bps: 'B/s', ms: 'ms', s: 'seconds' }[unit];
}

function compact(v: number): string {
  const abs = Math.abs(v);
  if (abs >= 1e9) return `${(v / 1e9).toFixed(1)}B`;
  if (abs >= 1e6) return `${(v / 1e6).toFixed(1)}M`;
  if (abs >= 1e3) return `${(v / 1e3).toFixed(1)}k`;
  if (abs === 0) return '0';
  if (abs < 1) return v.toFixed(2);
  if (abs < 10) return v.toFixed(1).replace(/\.0$/, ''); // 4.0 → 4, but keep 4.5
  return Math.round(v).toString();
}

export function formatBytes(bytes: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = bytes;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || Number.isInteger(v) ? 0 : 1)} ${units[i]}`;
}

function formatSeconds(s: number): string {
  if (s === 0) return '0 ms';
  if (s < 1) {
    const ms = s * 1000;
    // A decimal below 10ms, or an axis over a tight latency range shows duplicate ticks (2/2/1/1 ms).
    return `${ms < 10 ? ms.toFixed(1) : Math.round(ms)} ms`;
  }
  return `${s.toFixed(2)} s`;
}
