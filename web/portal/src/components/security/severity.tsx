// Shared severity visuals for the Security page: a stacked severity bar, inline count chips, and a
// single-severity badge. Every one pairs color with a text label, so meaning is never carried by
// color alone.

import { Group, Text, Tooltip, Box, useComputedColorScheme } from '@mantine/core';
import type { SecurityCounts, Severity } from '../../lib/types';
import { SEVERITIES, SEVERITY_META, countValue, countsTotal, severityColor, worstSeverity } from '../../lib/security';

// SeverityBar renders a breakdown as one horizontal stacked bar, each segment sized by its share and
// tooltipped with its label + count. A clean (zero-finding) breakdown shows a slim neutral track.
export function SeverityBar({ counts, height = 8 }: { counts: SecurityCounts; height?: number }) {
  const scheme = useComputedColorScheme('dark');
  const dark = scheme === 'dark';
  const total = countsTotal(counts);

  if (total === 0) {
    return (
      <Box
        style={{
          height,
          borderRadius: height,
          background: dark ? 'var(--mantine-color-dark-4)' : 'var(--mantine-color-gray-2)',
        }}
      />
    );
  }

  return (
    <Box style={{ display: 'flex', height, borderRadius: height, overflow: 'hidden', width: '100%' }}>
      {SEVERITIES.map((sev) => {
        const v = countValue(counts, sev);
        if (v === 0) return null;
        return (
          <Tooltip key={sev} label={`${SEVERITY_META[sev].label}: ${v}`} withArrow>
            <Box style={{ width: `${(v / total) * 100}%`, background: severityColor(sev, dark) }} />
          </Tooltip>
        );
      })}
    </Box>
  );
}

// SeverityChips renders one small count pill per non-zero severity (worst first). When every bucket
// is zero it shows a muted "No findings" so a clean workload reads as reassuring, not empty.
export function SeverityChips({ counts, size = 'sm' }: { counts: SecurityCounts; size?: 'xs' | 'sm' }) {
  const scheme = useComputedColorScheme('dark');
  const dark = scheme === 'dark';
  const total = countsTotal(counts);
  if (total === 0) {
    return (
      <Text size="xs" c="dimmed">
        No findings
      </Text>
    );
  }
  const fz = size === 'xs' ? 10 : 11;
  return (
    <Group gap={6} wrap="nowrap">
      {SEVERITIES.map((sev) => {
        const v = countValue(counts, sev);
        if (v === 0) return null;
        const color = severityColor(sev, dark);
        return (
          <Tooltip key={sev} label={SEVERITY_META[sev].label} withArrow>
            <Box
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 4,
                padding: '1px 7px',
                borderRadius: 999,
                fontSize: fz,
                fontWeight: 700,
                lineHeight: 1.6,
                color,
                border: `1px solid ${color}`,
                background: dark ? `${color}22` : `${color}18`,
              }}
            >
              <span>{SEVERITY_META[sev].short}</span>
              <span>{v}</span>
            </Box>
          </Tooltip>
        );
      })}
    </Group>
  );
}

// SeverityDot is a small labelled dot for a single severity (used in finding rows and legends).
export function SeverityDot({ severity, withLabel = true }: { severity: Severity; withLabel?: boolean }) {
  const scheme = useComputedColorScheme('dark');
  const color = severityColor(severity, scheme === 'dark');
  return (
    <Group gap={6} wrap="nowrap">
      <Box style={{ width: 9, height: 9, borderRadius: 999, background: color, flexShrink: 0 }} />
      {withLabel && (
        <Text size="sm" fw={600} style={{ color }}>
          {SEVERITY_META[severity].label}
        </Text>
      )}
    </Group>
  );
}

// SeverityLegend is a compact horizontal legend of every severity, for the top of a table/overview.
export function SeverityLegend() {
  return (
    <Group gap="md">
      {SEVERITIES.map((sev) => (
        <SeverityDot key={sev} severity={sev} />
      ))}
    </Group>
  );
}

// WorstBadge shows the single most-severe bucket present, as a filled pill - a one-glance risk label
// for a row. Clean breakdowns render a subtle "Clean" pill instead.
export function WorstBadge({ counts }: { counts: SecurityCounts }) {
  const scheme = useComputedColorScheme('dark');
  const dark = scheme === 'dark';
  const worst = worstSeverity(counts);
  if (!worst) {
    return (
      <Box
        style={{
          display: 'inline-block',
          padding: '2px 10px',
          borderRadius: 999,
          fontSize: 11,
          fontWeight: 700,
          color: dark ? 'var(--mantine-color-teal-4)' : 'var(--mantine-color-teal-7)',
          border: `1px solid ${dark ? 'var(--mantine-color-teal-7)' : 'var(--mantine-color-teal-4)'}`,
        }}
      >
        Clean
      </Box>
    );
  }
  const color = severityColor(worst, dark);
  return (
    <Box
      style={{
        display: 'inline-block',
        padding: '2px 10px',
        borderRadius: 999,
        fontSize: 11,
        fontWeight: 700,
        color: '#fff',
        background: color,
      }}
    >
      {SEVERITY_META[worst].label}
    </Box>
  );
}
