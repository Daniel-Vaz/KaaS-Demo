// Presentation helpers for the Security page: the per-cluster "trivy enabled" gate, severity
// ordering/labels/colors (a reserved, colorblind-distinguishable set - meaning is always paired with
// a label, never color alone), and small aggregation helpers over SecurityCounts.

import type { Cluster, SecurityCounts, Severity } from './types';

// SECURITY_ADDON is the catalog add-on that provides these reports (mirrors internal/security.AddonName).
export const SECURITY_ADDON = 'trivy-operator';

// securityEnabled mirrors internal/security.Enabled: the cluster has the Trivy Operator installed, so
// there are report CRDs to read. The server is the real gate; this just drives the page's gating UI.
export function securityEnabled(c: Cluster | undefined | null): boolean {
  return !!c?.addons?.some((a) => a.name === SECURITY_ADDON && a.phase === 'installed');
}

// SEVERITIES is severity order, most-severe first - the order bars, legends and columns render in.
export const SEVERITIES: Severity[] = ['critical', 'high', 'medium', 'low', 'unknown'];

// SEVERITY_META drives every severity chip/segment: a short label, a Mantine color name (for
// Badge/ThemeIcon), and explicit light/dark hex for the stacked bars (so hues stay distinct and
// legible in both themes). Distinct hues per severity, each always shown with its label.
export const SEVERITY_META: Record<
  Severity,
  { label: string; short: string; color: string; light: string; dark: string }
> = {
  critical: { label: 'Critical', short: 'C', color: 'red', light: '#c02026', dark: '#f2555a' },
  high: { label: 'High', short: 'H', color: 'orange', light: '#e07a24', dark: '#f0975a' },
  medium: { label: 'Medium', short: 'M', color: 'yellow', light: '#c99a12', dark: '#e6c02f' },
  low: { label: 'Low', short: 'L', color: 'blue', light: '#3f8fd0', dark: '#5aa6e0' },
  unknown: { label: 'Unknown', short: 'U', color: 'gray', light: '#8a8f98', dark: '#9aa0aa' },
};

export function severityColor(sev: Severity, dark: boolean): string {
  const m = SEVERITY_META[sev];
  return dark ? m.dark : m.light;
}

// countValue reads a single severity's bucket out of a breakdown.
export function countValue(c: SecurityCounts, sev: Severity): number {
  switch (sev) {
    case 'critical':
      return c.critical;
    case 'high':
      return c.high;
    case 'medium':
      return c.medium;
    case 'low':
      return c.low;
    default:
      return c.unknown;
  }
}

export function countsTotal(c: SecurityCounts | undefined | null): number {
  if (!c) return 0;
  return c.critical + c.high + c.medium + c.low + c.unknown;
}

export function sumCounts(list: SecurityCounts[]): SecurityCounts {
  return list.reduce(
    (acc, c) => ({
      critical: acc.critical + c.critical,
      high: acc.high + c.high,
      medium: acc.medium + c.medium,
      low: acc.low + c.low,
      unknown: acc.unknown + c.unknown,
    }),
    { critical: 0, high: 0, medium: 0, low: 0, unknown: 0 },
  );
}

// riskScore weights a breakdown so a single Critical outranks any pile of Lows - the order tables and
// the Overview surface "worst first". Mirrors the server-side riskScore.
export function riskScore(c: SecurityCounts): number {
  return c.critical * 1000 + c.high * 100 + c.medium * 10 + c.low;
}

// worstSeverity returns the most-severe non-zero bucket in a breakdown, or undefined when clean.
export function worstSeverity(c: SecurityCounts): Severity | undefined {
  for (const s of SEVERITIES) {
    if (countValue(c, s) > 0) return s;
  }
  return undefined;
}
