import { useEffect, useMemo } from 'react';
import { Link, useSearchParams } from 'react-router';
import { Group, Title, Text, Select, Tabs, Alert, Anchor, Badge } from '@mantine/core';
import { IconShieldHalf, IconAlertTriangle, IconServer2 } from '@tabler/icons-react';
import { useClusters, useSecurityMeta, useSecurityOverview } from '../lib/queries';
import { EmptyState } from '../components/EmptyState';
import { activeClusters, clusterUsable } from '../lib/cluster';
import { securityEnabled } from '../lib/security';
import { relative } from '../lib/format';
import { Overview } from '../components/security/Overview';
import { ReportsTable } from '../components/security/ReportsTable';

export function Security() {
  const [params, setParams] = useSearchParams();
  const { data: clustersRaw, isLoading: clustersLoading } = useClusters();
  const clusters = useMemo(() => activeClusters(clustersRaw ?? []), [clustersRaw]);

  // Selected cluster: from the URL, else the first usable cluster with Trivy, else the first usable,
  // else the first - mirroring the Monitoring page's resolution.
  const urlCluster = params.get('cluster') ?? '';
  const selected = clusters.find((c) => c.id === urlCluster);
  const fallback =
    clusters.find((c) => clusterUsable(c) && securityEnabled(c)) ??
    clusters.find(clusterUsable) ??
    clusters[0];
  const cluster = selected ?? fallback;
  const clusterId = cluster?.id;
  const ready = !!cluster && clusterUsable(cluster);

  const { data: meta } = useSecurityMeta(clusterId);
  const enabled = meta?.enabled ?? securityEnabled(cluster);
  const kinds = meta?.kinds ?? [];

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

  const tab = params.get('tab') || 'overview';
  const setTab = (v: string) =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        next.set('tab', v);
        return next;
      },
      { replace: true },
    );

  const active = !!ready && enabled;
  const { data: overview, isLoading: overviewLoading, error: overviewError } = useSecurityOverview(
    clusterId,
    active && tab === 'overview',
  );

  return (
    <>
      <Group justify="space-between" mb="lg" align="flex-start">
        <div>
          <Title order={2}>Security</Title>
          <Text c="dimmed" size="sm">
            Continuous vulnerability, misconfiguration, exposed-secret and RBAC scanning from the Trivy
            Operator running inside each cluster.
          </Text>
        </div>
        {overview && tab === 'overview' && (
          <Text size="xs" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
            updated {relative(overview.generated_at)}
          </Text>
        )}
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
          <Badge variant="light" color="grape" size="lg" leftSection={<IconShieldHalf size={14} />}>
            trivy-operator
          </Badge>
        )}
      </Group>

      {clusters.length === 0 && !clustersLoading ? (
        <EmptyState
          icon={IconServer2}
          title="No clusters yet"
          description="Create a cluster with the trivy-operator add-on - once it is Ready its security posture shows up here."
          action={
            <Anchor component={Link} to="/clusters/new" mt="sm">
              New cluster
            </Anchor>
          }
        />
      ) : cluster && !ready ? (
        <Alert color="yellow" icon={<IconAlertTriangle size={18} />} title={`${cluster.name} is not Ready`}>
          Security scanning is available once the cluster reaches <b>Ready</b> (currently <b>{cluster.phase}</b>).
          Watch it converge on its{' '}
          <Anchor component={Link} to={`/clusters/${cluster.id}`}>
            cluster page
          </Anchor>
          .
        </Alert>
      ) : cluster && !enabled ? (
        <EmptyState
          icon={IconShieldHalf}
          title="Security scanning not enabled"
          description="This cluster has no trivy-operator add-on installed, so there are no scan reports to read. Add the trivy-operator add-on to this cluster to light this page up."
          action={
            <Anchor component={Link} to={`/clusters/${cluster.id}`} mt="sm">
              Manage add-ons
            </Anchor>
          }
        />
      ) : (
        <Tabs value={tab} onChange={(v) => v && setTab(v)} keepMounted={false} variant="outline">
          <Tabs.List mb="md" style={{ flexWrap: 'nowrap', overflowX: 'auto', overflowY: 'hidden' }}>
            <Tabs.Tab value="overview">Overview</Tabs.Tab>
            {kinds.map((k) => (
              <Tabs.Tab key={k.id} value={k.id}>
                {k.title}
              </Tabs.Tab>
            ))}
          </Tabs.List>

          <Tabs.Panel value="overview">
            <Overview
              data={overview}
              isLoading={overviewLoading}
              error={overviewError}
              onSelectKind={(k) => setTab(k)}
            />
          </Tabs.Panel>

          {kinds.map((k) => (
            <Tabs.Panel key={k.id} value={k.id}>
              <ReportsTable clusterId={clusterId} kind={k.id} meta={k} enabled={active && tab === k.id} />
            </Tabs.Panel>
          ))}
        </Tabs>
      )}
    </>
  );
}
