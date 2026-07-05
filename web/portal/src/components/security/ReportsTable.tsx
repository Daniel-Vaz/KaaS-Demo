// The per-kind report table: one row per Trivy report CR, sorted worst-first, searchable and
// severity-filterable. Clicking a row opens the ReportDrawer with that report's findings. Image-backed
// kinds (vulnerability, exposed secret) show an image column; the others show the workload/RBAC object.

import { useMemo, useState } from 'react';
import {
  Table,
  Group,
  Text,
  TextInput,
  SegmentedControl,
  Skeleton,
  Alert,
  Badge,
  Box,
  useComputedColorScheme,
} from '@mantine/core';
import { IconSearch, IconAlertTriangle, IconShieldCheck } from '@tabler/icons-react';
import type { SecurityKind, SecurityKindMeta, SecurityReport, Severity } from '../../lib/types';
import { useSecurityReports } from '../../lib/queries';
import { ApiError } from '../../lib/api';
import { SEVERITIES, SEVERITY_META, countValue, countsTotal, riskScore } from '../../lib/security';
import { SeverityBar, SeverityChips } from './severity';
import { EmptyState } from '../EmptyState';
import { relative } from '../../lib/format';
import { ReportDrawer } from './ReportDrawer';

export function ReportsTable({
  clusterId,
  kind,
  meta,
  enabled,
}: {
  clusterId: string | undefined;
  kind: SecurityKind;
  meta: SecurityKindMeta;
  enabled: boolean;
}) {
  const scheme = useComputedColorScheme('dark');
  const { data, isLoading, error } = useSecurityReports(clusterId, kind, enabled);
  const [search, setSearch] = useState('');
  const [sev, setSev] = useState<Severity | 'all'>('all');
  const [selected, setSelected] = useState<SecurityReport | null>(null);
  const [open, setOpen] = useState(false);

  const rows = useMemo(() => {
    let r = data ?? [];
    if (sev !== 'all') r = r.filter((x) => countValue(x.summary, sev) > 0);
    const q = search.trim().toLowerCase();
    if (q) {
      r = r.filter(
        (x) =>
          x.resource.name.toLowerCase().includes(q) ||
          x.namespace.toLowerCase().includes(q) ||
          (x.artifact?.repository ?? '').toLowerCase().includes(q) ||
          (x.resource.container ?? '').toLowerCase().includes(q),
      );
    }
    return [...r].sort((a, b) => riskScore(b.summary) - riskScore(a.summary));
  }, [data, sev, search]);

  const openReport = (r: SecurityReport) => {
    setSelected(r);
    setOpen(true);
  };

  return (
    <>
      <Text size="sm" c="dimmed" mb="md">
        {meta.description}
      </Text>

      <Group mb="sm" gap="sm" wrap="wrap" justify="space-between">
        <Group gap="sm" wrap="wrap">
          <TextInput
            placeholder="Search reports…"
            leftSection={<IconSearch size={15} />}
            value={search}
            onChange={(e) => setSearch(e.currentTarget.value)}
            size="xs"
            w={240}
          />
          <SegmentedControl
            size="xs"
            value={sev}
            onChange={(v) => setSev(v as Severity | 'all')}
            data={[{ value: 'all', label: 'All' }, ...SEVERITIES.map((s) => ({ value: s, label: SEVERITY_META[s].label }))]}
          />
        </Group>
        {data && (
          <Text size="xs" c="dimmed">
            {rows.length} of {data.length} report{data.length === 1 ? '' : 's'}
          </Text>
        )}
      </Group>

      {error ? (
        <Alert color="red" icon={<IconAlertTriangle size={16} />} title="Could not load reports">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !data ? (
        <Skeleton height={280} radius="md" />
      ) : (data?.length ?? 0) === 0 ? (
        <EmptyState
          icon={IconShieldCheck}
          title="No reports yet"
          description={`Trivy hasn't published any ${meta.title.toLowerCase()} for this cluster yet - the operator scans continuously, so give it a minute after the cluster becomes Ready.`}
        />
      ) : rows.length === 0 ? (
        <EmptyState icon={IconSearch} title="No matching reports" description="Try a different search or severity filter." />
      ) : (
        <Table.ScrollContainer minWidth={720}>
          <Table highlightOnHover verticalSpacing="sm" striped={scheme === 'light'}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{meta.has_artifact ? 'Image' : 'Resource'}</Table.Th>
                <Table.Th>{meta.has_artifact ? 'Workload' : 'Namespace'}</Table.Th>
                <Table.Th>Findings</Table.Th>
                <Table.Th w={160}>Severity</Table.Th>
                <Table.Th w={110}>Scanned</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((r) => (
                <Table.Tr
                  key={`${r.namespace}/${r.name}`}
                  onClick={() => openReport(r)}
                  style={{ cursor: 'pointer' }}
                >
                  <Table.Td>
                    {meta.has_artifact ? (
                      <ImageCell report={r} />
                    ) : (
                      <div>
                        <Text size="sm" fw={600}>
                          {r.resource.name}
                        </Text>
                        <Text size="xs" c="dimmed">
                          {r.resource.kind}
                        </Text>
                      </div>
                    )}
                  </Table.Td>
                  <Table.Td>
                    {meta.has_artifact ? (
                      <div>
                        <Text size="sm">{r.resource.name}</Text>
                        <Group gap={6}>
                          <Text size="xs" c="dimmed">
                            {r.namespace}
                          </Text>
                          {r.resource.container && (
                            <Badge size="xs" variant="light" color="gray">
                              {r.resource.container}
                            </Badge>
                          )}
                        </Group>
                      </div>
                    ) : (
                      <Badge variant="outline" color="gray">
                        {r.namespace || 'cluster-scoped'}
                      </Badge>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <SeverityChips counts={r.summary} />
                  </Table.Td>
                  <Table.Td>
                    <Box w={150}>
                      <SeverityBar counts={r.summary} />
                      <Text size="10px" c="dimmed" mt={4}>
                        {countsTotal(r.summary)} {meta.finding_noun}
                        {countsTotal(r.summary) === 1 ? '' : 's'}
                      </Text>
                    </Box>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" c="dimmed">
                      {r.updated_at ? relative(r.updated_at) : '-'}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <ReportDrawer clusterId={clusterId} kind={kind} report={selected} opened={open} onClose={() => setOpen(false)} />
    </>
  );
}

function ImageCell({ report }: { report: SecurityReport }) {
  const a = report.artifact;
  if (!a) return <Text size="sm">-</Text>;
  const short = a.repository.split('/').pop() ?? a.repository;
  return (
    <div style={{ minWidth: 0 }}>
      <Text size="sm" fw={600} style={{ fontFamily: 'var(--mantine-font-family-monospace)' }}>
        {short}
        {a.tag ? `:${a.tag}` : ''}
      </Text>
      <Text size="xs" c="dimmed" truncate>
        {a.registry ? `${a.registry}/` : ''}
        {a.repository}
      </Text>
    </div>
  );
}
