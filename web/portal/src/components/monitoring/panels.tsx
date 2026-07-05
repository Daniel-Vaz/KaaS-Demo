// The Monitoring page's panel primitives: a Panel dispatcher that renders a resolved PanelResult by
// its kind, plus the individual panels (SLO ring, utilisation gauge, stat tile with optional
// sparkline, line/area/stacked time-series, top-k bar list, a control-plane status grid, and an
// active-alerts list). Built on Mantine + @mantine/charts, themed for light and dark, following the
// dataviz design system (fixed-order CVD-safe series colors, reserved status colors paired with an
// icon+label, single axis, a legend for ≥2 series, ~10% area washes, rounded data-ends).

import {
  Card,
  Group,
  Text,
  Tooltip,
  RingProgress,
  Progress,
  SimpleGrid,
  Paper,
  ThemeIcon,
  Badge,
  Stack,
  Center,
  useComputedColorScheme,
} from '@mantine/core';
import { LineChart, AreaChart, Sparkline } from '@mantine/charts';
import {
  IconCircleCheckFilled,
  IconCircleXFilled,
  IconAlertTriangleFilled,
  IconBellRinging,
  IconShieldCheck,
  IconInfoCircle,
} from '@tabler/icons-react';
import type { PanelResult, MonitoringSeries, MonitoringAlert } from '../../lib/types';
import {
  formatValue,
  formatAxis,
  unitLabel,
  seriesColor,
  gaugeColor,
  sloColor,
  severityColor,
  severityRank,
} from '../../lib/monitoring';
import { relative } from '../../lib/format';

// panelSpan is the 12-column grid span for a panel: small scalars sit in a KPI row, charts go
// half-width, and the status grid / alerts list span the full row. A featured gauge (cluster
// CPU/memory on Overview) gets a much bigger span than a regular quarter-tile gauge, so it stands
// out at the top of its tab.
export function panelSpan(p: Pick<PanelResult, 'kind' | 'featured'>): { base: number; sm?: number; md?: number; lg?: number } {
  // Featured = the standout treatment for the kind: a gauge becomes a half-width hero ring, a
  // timeseries goes full-width (composition charts like CPU-by-namespace want the room).
  if (p.featured) return p.kind === 'timeseries' ? { base: 12 } : { base: 12, sm: 6 };
  switch (p.kind) {
    case 'slo':
      return { base: 12, sm: 6, md: 4 };
    case 'stat':
    case 'gauge':
      return { base: 6, sm: 4, md: 3 };
    case 'timeseries':
    case 'bars':
      return { base: 12, md: 6 };
    default: // status | alerts
      return { base: 12 };
  }
}

export function Panel({ p }: { p: PanelResult }) {
  switch (p.kind) {
    case 'slo':
      return <SloPanel p={p} />;
    case 'gauge':
      return p.featured ? <HeroGaugePanel p={p} /> : <GaugePanel p={p} />;
    case 'stat':
      return <StatPanel p={p} />;
    case 'timeseries':
      return <TimeSeriesPanel p={p} />;
    case 'bars':
      return <BarsPanel p={p} />;
    case 'status':
      return <StatusGridPanel p={p} />;
    case 'alerts':
      return <AlertsPanel p={p} />;
    default:
      return null;
  }
}

// PanelFrame is the shared card chrome: title with an optional info tooltip (the panel's desc),
// optional right-slot, and soft error/empty handling.
function PanelFrame({
  title,
  desc,
  right,
  error,
  minH,
  children,
}: {
  title: string;
  desc?: string;
  right?: React.ReactNode;
  error?: string;
  minH?: number;
  children: React.ReactNode;
}) {
  return (
    <Card padding="md" radius="md" h="100%">
      <Group justify="space-between" mb="sm" wrap="nowrap" gap="xs">
        <Group gap={6} wrap="nowrap" style={{ minWidth: 0 }}>
          <Text fw={600} size="sm" lineClamp={1}>
            {title}
          </Text>
          {desc && (
            <Tooltip label={desc} multiline maw={320} withArrow position="top" events={{ hover: true, focus: true, touch: true }}>
              <Center component="span" c="dimmed" style={{ flexShrink: 0, cursor: 'help' }}>
                <IconInfoCircle size={14} aria-label="About this panel" />
              </Center>
            </Tooltip>
          )}
        </Group>
        {right}
      </Group>
      {error ? (
        <Center mih={minH ?? 80}>
          <Text size="xs" c="dimmed">
            {error === 'no data' ? 'No data yet' : `Unavailable - ${error}`}
          </Text>
        </Center>
      ) : (
        children
      )}
    </Card>
  );
}

function SloPanel({ p }: { p: PanelResult }) {
  const v = p.value ?? 0;
  const target = p.target ?? 0.995;
  const color = sloColor(v, target);
  return (
    <PanelFrame title={p.title} desc={p.desc} error={p.error} minH={160}>
      <Center>
        <RingProgress
          size={168}
          thickness={14}
          roundCaps
          sections={[{ value: Math.min(100, v * 100), color }]}
          label={
            <div style={{ textAlign: 'center' }}>
              <Text fw={700} size="1.6rem" lh={1.1}>
                {formatValue(v, 'ratio')}
              </Text>
              <Text size="xs" c="dimmed">
                target {formatValue(target, 'ratio')}
              </Text>
            </div>
          }
        />
      </Center>
    </PanelFrame>
  );
}

// HeroGaugePanel is the standout treatment for a Featured gauge (cluster CPU/memory on Overview): a
// big ring - the same visual language as the SLO panel - instead of the thin linear bar a regular
// GaugePanel uses, so these two lead the tab and read as the headline numbers they are.
function HeroGaugePanel({ p }: { p: PanelResult }) {
  const v = p.value ?? 0;
  const pctVal = Math.round(Math.min(1, Math.max(0, v)) * 100);
  const color = gaugeColor(v);
  return (
    <PanelFrame title={p.title} desc={p.desc} error={p.error} minH={160}>
      <Center>
        <RingProgress
          size={188}
          thickness={16}
          roundCaps
          sections={[{ value: pctVal, color }]}
          label={
            <div style={{ textAlign: 'center' }}>
              <Text fw={700} size="1.8rem" lh={1.1}>
                {pctVal}%
              </Text>
              <Text size="xs" c="dimmed">
                utilised
              </Text>
            </div>
          }
        />
      </Center>
    </PanelFrame>
  );
}

function GaugePanel({ p }: { p: PanelResult }) {
  const v = p.value ?? 0;
  const pctVal = Math.round(Math.min(1, Math.max(0, v)) * 100);
  const color = gaugeColor(v);
  const hot = v >= 0.9;
  return (
    <PanelFrame
      title={p.title}
      desc={p.desc}
      error={p.error}
      right={
        <Text size="sm" c="dimmed" fw={600}>
          {pctVal}%
        </Text>
      }
    >
      <Progress value={pctVal} color={color} size="lg" radius="xl" striped={hot} animated={hot} mt={4} />
    </PanelFrame>
  );
}

// StatPanel is a KPI tile: the headline value, plus - when the server sent a series alongside it (a
// "sparkline stat", KindStat + Range) - the selected window's trend as a soft sparkline underneath.
function StatPanel({ p }: { p: PanelResult }) {
  const dark = useComputedColorScheme('dark') === 'dark';
  const spark = (p.series ?? [])[0]?.points ?? [];
  return (
    <PanelFrame
      title={p.title}
      desc={p.desc}
      error={p.error}
      right={
        unitLabel(p.unit) ? (
          <Text size="xs" c="dimmed">
            {unitLabel(p.unit)}
          </Text>
        ) : undefined
      }
    >
      <Text fw={700} size="1.9rem" lh={1.1}>
        {p.value === undefined ? '-' : formatValue(p.value, p.unit)}
      </Text>
      {spark.length > 1 && (
        <Sparkline
          h={36}
          mt={6}
          data={spark.map((pt) => pt.v)}
          color={seriesColor(0, dark)}
          fillOpacity={0.12}
          strokeWidth={1.5}
          curveType="monotone"
        />
      )}
    </PanelFrame>
  );
}

function TimeSeriesPanel({ p }: { p: PanelResult }) {
  const dark = useComputedColorScheme('dark') === 'dark';
  const series = p.series ?? [];
  const hasData = series.some((s) => s.points.length > 0);
  const stacked = p.viz === 'stacked';
  // Stacking needs every series defined at every timestamp - a hole in one series would tear the
  // bands - so zero-fill only in that mode.
  const { rows, names } = toChartData(series, stacked);
  const chartSeries = names.map((name, i) => ({ name, color: seriesColor(i, dark) }));
  const shared = {
    h: 220,
    data: rows,
    dataKey: 'time',
    series: chartSeries,
    curveType: 'monotone',
    withDots: false,
    strokeWidth: 2,
    withLegend: chartSeries.length > 1,
    // Past ~4 entries the legend wraps to a second line - give it room or entries get clipped.
    legendProps: { verticalAlign: 'bottom', height: chartSeries.length > 4 ? 56 : 32 },
    tickLine: 'x',
    gridAxis: 'y',
    valueFormatter: (v: number) => formatValue(v, p.unit),
    yAxisProps: { width: 52, tickFormatter: (v: number) => formatAxis(v, p.unit) },
    xAxisProps: { minTickGap: 40 },
  } as const;
  return (
    <PanelFrame
      title={p.title}
      desc={p.desc}
      error={p.error || (!hasData ? 'no data' : undefined)}
      minH={220}
      right={
        unitLabel(p.unit) ? (
          <Text size="xs" c="dimmed">
            {unitLabel(p.unit)}
          </Text>
        ) : undefined
      }
    >
      {p.viz === 'area' || stacked ? (
        // Area for single-series throughput (a soft wash under the line), stacked for part-to-whole
        // composition (rate by code, CPU by namespace). Stacked bands get a bit more fill so the
        // composition reads; the plain area stays a ~10% wash.
        <AreaChart {...shared} type={stacked ? 'stacked' : 'default'} fillOpacity={stacked ? 0.3 : 0.12} />
      ) : (
        <LineChart {...shared} />
      )}
    </PanelFrame>
  );
}

// BarsPanel is a top-k horizontal bar list (usage by namespace, restarts by pod): label + value per
// row, bar length proportional to the largest row. Magnitude comparison of one measure → a single
// hue (categorical slot 1), per the dataviz form rules; an empty list is the healthy state, not an
// error ("no pods restarted").
function BarsPanel({ p }: { p: PanelResult }) {
  const dark = useComputedColorScheme('dark') === 'dark';
  const bars = p.bars ?? [];
  const maxV = Math.max(...bars.map((b) => b.value), 0);
  const color = seriesColor(0, dark);
  return (
    <PanelFrame title={p.title} desc={p.desc} error={p.error} minH={120}>
      {bars.length === 0 ? (
        <Group gap={8} c="dimmed" py="sm">
          <IconShieldCheck size={18} />
          <Text size="sm">Nothing to report in this window.</Text>
        </Group>
      ) : (
        <Stack gap={10}>
          {bars.map((b) => (
            <div key={b.name}>
              <Group justify="space-between" wrap="nowrap" gap="xs" mb={3}>
                <Text size="xs" lineClamp={1} style={{ minWidth: 0 }}>
                  {b.name}
                </Text>
                <Text size="xs" fw={600} ff="monospace" style={{ whiteSpace: 'nowrap' }}>
                  {formatValue(b.value, p.unit)}
                </Text>
              </Group>
              <div
                aria-hidden
                style={{
                  height: 8,
                  borderRadius: 4,
                  background: 'var(--mantine-color-default-hover)',
                }}
              >
                <div
                  style={{
                    width: `${maxV > 0 ? Math.max((b.value / maxV) * 100, 1.5) : 0}%`,
                    height: '100%',
                    background: color,
                    // Rounded data-end, square at the baseline (left edge).
                    borderRadius: '2px 4px 4px 2px',
                  }}
                />
              </div>
            </div>
          ))}
        </Stack>
      )}
    </PanelFrame>
  );
}

function StatusGridPanel({ p }: { p: PanelResult }) {
  const rows = p.rows ?? [];
  const down = rows.filter((r) => !r.up).length;
  return (
    <PanelFrame
      title={p.title}
      desc={p.desc}
      error={p.error || (rows.length === 0 ? 'no data' : undefined)}
      right={
        <Badge color={down === 0 ? 'teal' : 'red'} variant="light" size="sm">
          {down === 0 ? 'All up' : `${down} down`}
        </Badge>
      }
    >
      <SimpleGrid cols={{ base: 2, sm: 3, md: 4 }} spacing="sm">
        {rows.map((r) => (
          <Paper key={r.label} p="sm" radius="md" withBorder>
            <Group gap={8} wrap="nowrap">
              <ThemeIcon variant="light" color={r.up ? 'teal' : 'red'} radius="xl" size="md">
                {r.up ? <IconCircleCheckFilled size={18} /> : <IconCircleXFilled size={18} />}
              </ThemeIcon>
              <div style={{ minWidth: 0 }}>
                <Text size="sm" fw={600} lineClamp={1}>
                  {r.label}
                </Text>
                <Text size="xs" c="dimmed" lineClamp={1}>
                  {r.detail || (r.up ? 'up' : 'down')}
                </Text>
              </div>
            </Group>
          </Paper>
        ))}
      </SimpleGrid>
    </PanelFrame>
  );
}

function AlertsPanel({ p }: { p: PanelResult }) {
  const alerts = [...(p.alerts ?? [])].sort(
    (a, b) => severityRank(a.severity) - severityRank(b.severity) || a.name.localeCompare(b.name),
  );
  // "Actionable" = anything beyond the always-firing Watchdog / info-level noise.
  const actionable = alerts.filter((a) => a.severity === 'critical' || a.severity === 'warning');
  return (
    <PanelFrame
      title={p.title}
      desc={p.desc}
      error={p.error}
      right={
        <Badge color={actionable.length ? 'red' : 'teal'} variant="light" size="sm">
          {actionable.length ? `${actionable.length} active` : 'Healthy'}
        </Badge>
      }
    >
      {alerts.length === 0 ? (
        <Group gap={8} c="dimmed" py="sm">
          <IconShieldCheck size={18} />
          <Text size="sm">No alerts firing.</Text>
        </Group>
      ) : (
        <Stack gap="xs">
          {alerts.map((a, i) => (
            <AlertRow key={`${a.name}-${i}`} a={a} />
          ))}
        </Stack>
      )}
    </PanelFrame>
  );
}

function AlertRow({ a }: { a: MonitoringAlert }) {
  const color = severityColor(a.severity);
  const Icon = a.severity === 'critical' || a.severity === 'warning' ? IconAlertTriangleFilled : IconBellRinging;
  return (
    <Paper p="sm" radius="md" withBorder>
      <Group justify="space-between" wrap="nowrap" gap="sm">
        <Group gap={8} wrap="nowrap" style={{ minWidth: 0 }}>
          <ThemeIcon variant="light" color={color} radius="xl" size="md">
            <Icon size={16} />
          </ThemeIcon>
          <div style={{ minWidth: 0 }}>
            <Group gap={6} wrap="nowrap">
              <Text size="sm" fw={600} lineClamp={1}>
                {a.name}
              </Text>
              <Badge size="xs" variant="light" color={color}>
                {a.severity}
              </Badge>
              <Badge size="xs" variant="outline" color={a.state === 'firing' ? color : 'gray'}>
                {a.state}
              </Badge>
            </Group>
            <Text size="xs" c="dimmed" lineClamp={2}>
              {a.summary || '-'}
            </Text>
          </div>
        </Group>
        {a.active_at && (
          <Text size="xs" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
            {relative(a.active_at)}
          </Text>
        )}
      </Group>
    </Paper>
  );
}

// toChartData zips per-series points into recharts rows keyed by timestamp, with an HH:mm x label.
// fillZero backfills a missing series value with 0 on every row - required for stacked areas, where
// an undefined sample would tear the stack.
function toChartData(
  series: MonitoringSeries[],
  fillZero = false,
): { rows: Record<string, number | string>[]; names: string[] } {
  const names = series.map((s) => s.name);
  const byT = new Map<number, Record<string, number | string>>();
  for (const s of series) {
    for (const pt of s.points) {
      let row = byT.get(pt.t);
      if (!row) {
        row = { t: pt.t, time: hhmm(pt.t) };
        byT.set(pt.t, row);
      }
      row[s.name] = pt.v;
    }
  }
  const rows = [...byT.values()].sort((a, b) => (a.t as number) - (b.t as number));
  if (fillZero) {
    for (const row of rows) {
      for (const name of names) {
        if (row[name] === undefined) row[name] = 0;
      }
    }
  }
  return { rows, names };
}

function hhmm(unixSeconds: number): string {
  const d = new Date(unixSeconds * 1000);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}
