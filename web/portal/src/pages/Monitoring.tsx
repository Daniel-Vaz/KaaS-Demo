import { useEffect, useMemo } from 'react';
import { Link, useSearchParams } from 'react-router';
import {
  Group,
  Title,
  Text,
  Select,
  Tabs,
  Grid,
  Skeleton,
  Alert,
  Anchor,
  Badge,
  Button,
  Divider,
} from '@mantine/core';
import {
  IconChartAreaLine,
  IconAlertTriangle,
  IconServer2,
  IconChartHistogram,
  IconClock,
  IconExternalLink,
} from '@tabler/icons-react';
import { useClusters, useMonitoringMeta, useMonitoringTab } from '../lib/queries';
import { ApiError } from '../lib/api';
import { EmptyState } from '../components/EmptyState';
import { activeClusters, clusterUsable } from '../lib/cluster';
import { monitoringEnabled } from '../lib/monitoring';
import { relative } from '../lib/format';
import { Panel, panelSpan } from '../components/monitoring/panels';
import type { PanelResult } from '../lib/types';

// groupBySection chunks a tab's panels into consecutive runs that share a section title (the server
// orders panels so a section's panels are adjacent). Panels without a section render header-less.
function groupBySection(panels: PanelResult[]): { section: string; panels: PanelResult[] }[] {
  const groups: { section: string; panels: PanelResult[] }[] = [];
  for (const p of panels) {
    const last = groups[groups.length - 1];
    if (last && last.section === (p.section ?? '')) {
      last.panels.push(p);
    } else {
      groups.push({ section: p.section ?? '', panels: [p] });
    }
  }
  return groups;
}

// Fallback picker windows, used only until the server's meta (its authoritative list + default)
// loads - mirrors internal/monitoring.Ranges / DefaultRange.
const FALLBACK_RANGES = ['5m', '15m', '30m', '1h', '3h', '12h'];
const FALLBACK_DEFAULT_RANGE = '15m';

// rangeLabel renders a picker window ("15m", "1h") as a short human label ("Last 15m", "Last 1h").
function rangeLabel(r: string): string {
  return `Last ${r}`;
}

export function Monitoring() {
  const [params, setParams] = useSearchParams();
  const { data: clustersRaw, isLoading: clustersLoading } = useClusters();
  const clusters = useMemo(() => activeClusters(clustersRaw ?? []), [clustersRaw]);

  // Selected cluster: from the URL, else the first usable cluster with monitoring, else the first
  // usable cluster, else the first.
  const urlCluster = params.get('cluster') ?? '';
  const selected = clusters.find((c) => c.id === urlCluster);
  const fallback =
    clusters.find((c) => clusterUsable(c) && monitoringEnabled(c)) ??
    clusters.find(clusterUsable) ??
    clusters[0];
  const cluster = selected ?? fallback;
  const clusterId = cluster?.id;
  const ready = !!cluster && clusterUsable(cluster);

  const { data: meta } = useMonitoringMeta(clusterId);
  // Prefer the server's authoritative flag once loaded; fall back to the cluster's add-on list so the
  // gating UI is correct on the first paint.
  const enabled = meta?.enabled ?? monitoringEnabled(cluster);
  const tabs = meta?.tabs ?? [];

  // "Open UI" links: drop the write-scoped ones (Alertmanager) for actors who can't manage this
  // cluster - a read-role group-mate would only get a 403 from the API's own gate.
  const visibleApps = useMemo(
    () => (meta?.apps ?? []).filter((a) => !a.write_scoped || (cluster?.can_manage ?? false)),
    [meta?.apps, cluster?.can_manage],
  );

  // Keep the URL in sync with the resolved cluster so the page is deep-linkable and stable on reload.
  useEffect(() => {
    if (cluster && cluster.id !== urlCluster) {
      setParams(
        (p) => {
          const next = new URLSearchParams(p);
          next.set('cluster', cluster.id);
          return next;
        },
        { replace: true },
      );
    }
  }, [cluster, urlCluster, setParams]);

  const tab = params.get('tab') || tabs[0]?.id || 'overview';
  const setTab = (v: string) =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        next.set('tab', v);
        return next;
      },
      { replace: true },
    );

  // Time-range window: one selection for the whole page, deep-linked in the URL and defaulting to the
  // server's DefaultRange (last 15m). Every range panel on every tab respects it.
  const ranges = meta?.ranges ?? FALLBACK_RANGES;
  const defaultRange = meta?.default_range ?? FALLBACK_DEFAULT_RANGE;
  const range = params.get('range') || defaultRange;
  const setRange = (v: string) =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        next.set('range', v);
        return next;
      },
      { replace: true },
    );

  const { data: tabData, isLoading, error } = useMonitoringTab(clusterId, tab, range, !!ready && enabled);

  return (
    <>
      <Group justify="space-between" mb="lg" align="flex-start">
        <div>
          <Title order={2}>Monitoring</Title>
          <Text c="dimmed" size="sm">
            Live cluster telemetry from Prometheus - availability SLOs, control-plane health, resource
            saturation, workload usage, and firing alerts.
          </Text>
        </div>
        <Group gap="sm" align="center" wrap="nowrap">
          {ready && enabled && (
            <Select
              aria-label="Time range"
              data={ranges.map((r) => ({ value: r, label: rangeLabel(r) }))}
              value={range}
              onChange={(v) => v && setRange(v)}
              leftSection={<IconClock size={16} />}
              size="sm"
              w={140}
              allowDeselect={false}
              comboboxProps={{ withinPortal: true }}
            />
          )}
          {tabData && (
            <Text size="xs" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
              updated {relative(tabData.generated_at)}
            </Text>
          )}
        </Group>
      </Group>

      <Group mb="md" gap="sm" align="flex-end" wrap="wrap">
        <Select
          label="Cluster"
          placeholder={clustersLoading ? 'Loading…' : 'Select a cluster'}
          data={clusters.map((c) => ({
            value: c.id,
            label: clusterUsable(c) ? c.name : `${c.name} (${c.phase})`,
          }))}
          value={clusterId ?? null}
          onChange={(v) => {
            if (v)
              setParams((p) => {
                const next = new URLSearchParams(p);
                next.set('cluster', v);
                return next;
              });
          }}
          searchable
          w={260}
          nothingFoundMessage="No clusters"
        />
        {cluster && enabled && (
          <Badge variant="light" color="teal" size="lg" leftSection={<IconChartHistogram size={14} />}>
            kube-prometheus-stack
          </Badge>
        )}
      </Group>

      {/* Open the stack's own web UIs (Grafana/Prometheus/Alertmanager) in a new tab. They have no
          ingress; each link is reverse-proxied per cluster through the control plane (see internal/tunnel).
          A write_scoped app (Alertmanager - its UI silences alerts, and it has no auth of its own) is
          hidden without manage access; the API still enforces it with a 403. Grafana signs the user in
          with a portal-derived role (Read → Viewer, Write → Editor), so it needs no gating here. */}
      {ready && enabled && clusterId && (visibleApps?.length ?? 0) > 0 && (
        <Group mb="md" gap="xs" align="center">
          <Text size="sm" c="dimmed">
            Open UI:
          </Text>
          {visibleApps.map((appLink) => (
            <Button
              key={appLink.id}
              component="a"
              href={`/api/clusters/${clusterId}/proxy/${appLink.id}/`}
              target="_blank"
              rel="noopener noreferrer"
              variant="light"
              size="xs"
              rightSection={<IconExternalLink size={14} />}
            >
              {appLink.name}
            </Button>
          ))}
        </Group>
      )}

      {clusters.length === 0 && !clustersLoading ? (
        <EmptyState
          icon={IconServer2}
          title="No clusters yet"
          description="Create a cluster with the monitoring add-on - once it is Ready its telemetry shows up here."
          action={
            <Anchor component={Link} to="/clusters/new" mt="sm">
              New cluster
            </Anchor>
          }
        />
      ) : cluster && !ready ? (
        <Alert color="yellow" icon={<IconAlertTriangle size={18} />} title={`${cluster.name} is not Ready`}>
          Monitoring is available once the cluster reaches <b>Ready</b> (currently <b>{cluster.phase}</b>).
          Watch it converge on its{' '}
          <Anchor component={Link} to={`/clusters/${cluster.id}`}>
            cluster page
          </Anchor>
          .
        </Alert>
      ) : cluster && !enabled ? (
        <EmptyState
          icon={IconChartAreaLine}
          title="Monitoring not enabled"
          description="This cluster has no kube-prometheus-stack add-on installed, so there is no Prometheus to query. Add the monitoring add-on to this cluster to light this page up."
          action={
            <Anchor component={Link} to={`/clusters/${cluster.id}`} mt="sm">
              Manage add-ons
            </Anchor>
          }
        />
      ) : (
        <Tabs value={tab} onChange={(v) => v && setTab(v)} keepMounted={false} variant="outline">
          <Tabs.List mb="md" style={{ flexWrap: 'nowrap', overflowX: 'auto', overflowY: 'hidden' }}>
            {tabs.map((t) => (
              <Tabs.Tab key={t.id} value={t.id}>
                {t.title}
              </Tabs.Tab>
            ))}
          </Tabs.List>

          {error ? (
            <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Could not load monitoring data">
              {error instanceof ApiError ? error.message : String(error)}
            </Alert>
          ) : isLoading && !tabData ? (
            <Grid>
              {[0, 1, 2, 3, 4, 5].map((i) => (
                <Grid.Col key={i} span={{ base: 12, sm: 6, md: 4 }}>
                  <Skeleton height={160} radius="md" />
                </Grid.Col>
              ))}
            </Grid>
          ) : (
            groupBySection(tabData?.panels ?? []).map((g, gi) => (
              <div key={g.section || `group-${gi}`}>
                {g.section && (
                  <Divider
                    mt={gi === 0 ? 0 : 'lg'}
                    mb="sm"
                    labelPosition="left"
                    label={
                      <Text size="xs" fw={700} tt="uppercase" c="dimmed" lts={0.6}>
                        {g.section}
                      </Text>
                    }
                  />
                )}
                <Grid mb="xs">
                  {g.panels.map((p) => (
                    <Grid.Col key={p.id} span={panelSpan(p)}>
                      <Panel p={p} />
                    </Grid.Col>
                  ))}
                </Grid>
              </div>
            ))
          )}
        </Tabs>
      )}
    </>
  );
}
