// The per-report detail drawer: opens when a row in a ReportsTable is clicked and streams that
// report's full finding list. Findings are searchable and severity-filterable, and rendered per-kind
// (a CVE shows package/version/fix/score; a config or RBAC check shows category + remediation; an
// exposed secret shows the file + redacted match).

import { useMemo, useState } from 'react';
import {
  Drawer,
  Group,
  Text,
  Badge,
  Stack,
  TextInput,
  SegmentedControl,
  ScrollArea,
  Anchor,
  Code,
  Divider,
  Skeleton,
  Alert,
  Box,
} from '@mantine/core';
import { IconSearch, IconExternalLink, IconAlertTriangle, IconShieldCheck } from '@tabler/icons-react';
import type { SecurityKind, SecurityReport, SecurityFinding, Severity } from '../../lib/types';
import { useSecurityReport } from '../../lib/queries';
import { ApiError } from '../../lib/api';
import { SEVERITY_META, SEVERITIES, severityColor } from '../../lib/security';
import { SeverityChips } from './severity';
import { relative } from '../../lib/format';
import { useComputedColorScheme } from '@mantine/core';

export function ReportDrawer({
  clusterId,
  kind,
  report,
  opened,
  onClose,
}: {
  clusterId: string | undefined;
  kind: SecurityKind;
  report: SecurityReport | null;
  opened: boolean;
  onClose: () => void;
}) {
  const [search, setSearch] = useState('');
  const [sev, setSev] = useState<Severity | 'all'>('all');

  const { data, isLoading, error } = useSecurityReport(
    clusterId,
    kind,
    report?.namespace ?? '',
    report?.name ?? '',
    opened && !!report,
  );

  const findings = useMemo(() => {
    let f = data?.findings ?? [];
    if (sev !== 'all') f = f.filter((x) => x.severity === sev);
    const q = search.trim().toLowerCase();
    if (q) {
      f = f.filter(
        (x) =>
          x.id.toLowerCase().includes(q) ||
          x.title.toLowerCase().includes(q) ||
          (x.resource ?? '').toLowerCase().includes(q) ||
          (x.category ?? '').toLowerCase().includes(q),
      );
    }
    return f;
  }, [data, sev, search]);

  const title = report
    ? report.artifact?.repository
      ? report.artifact.repository.split('/').pop() + (report.artifact.tag ? `:${report.artifact.tag}` : '')
      : `${report.resource.kind}/${report.resource.name}`
    : '';

  return (
    <Drawer
      opened={opened}
      onClose={onClose}
      position="right"
      size="xl"
      title={
        <Group gap="xs">
          <Text fw={700} size="lg">
            {title}
          </Text>
          {report?.resource.container && (
            <Badge variant="light" color="gray" size="sm">
              {report.resource.container}
            </Badge>
          )}
        </Group>
      }
      scrollAreaComponent={ScrollArea.Autosize}
    >
      {report && (
        <Stack gap="sm" mb="md">
          <Group gap="xs" wrap="wrap">
            <Badge variant="outline" color="gray">
              {report.namespace || 'cluster-scoped'}
            </Badge>
            <Badge variant="light" color="gray">
              {report.resource.kind} · {report.resource.name}
            </Badge>
            {report.scanner && (
              <Text size="xs" c="dimmed">
                {report.scanner}
              </Text>
            )}
            {report.updated_at && (
              <Text size="xs" c="dimmed">
                scanned {relative(report.updated_at)}
              </Text>
            )}
          </Group>
          {report.artifact && (
            <Code style={{ fontSize: 12 }}>
              {report.artifact.registry ? `${report.artifact.registry}/` : ''}
              {report.artifact.repository}
              {report.artifact.tag ? `:${report.artifact.tag}` : ''}
            </Code>
          )}
          <SeverityChips counts={report.summary} />
        </Stack>
      )}

      <Divider mb="md" />

      <Group mb="md" gap="sm" wrap="wrap">
        <TextInput
          placeholder="Filter findings…"
          leftSection={<IconSearch size={15} />}
          value={search}
          onChange={(e) => setSearch(e.currentTarget.value)}
          size="xs"
          style={{ flex: 1, minWidth: 180 }}
        />
        <SegmentedControl
          size="xs"
          value={sev}
          onChange={(v) => setSev(v as Severity | 'all')}
          data={[
            { value: 'all', label: 'All' },
            ...SEVERITIES.filter((s) => (data?.findings ?? []).some((f) => f.severity === s)).map((s) => ({
              value: s,
              label: SEVERITY_META[s].label,
            })),
          ]}
        />
      </Group>

      {error ? (
        <Alert color="red" icon={<IconAlertTriangle size={16} />}>
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !data ? (
        <Stack>
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} height={72} radius="md" />
          ))}
        </Stack>
      ) : findings.length === 0 ? (
        <Group justify="center" py="xl" c="dimmed" gap="xs">
          <IconShieldCheck size={20} />
          <Text size="sm">No findings match.</Text>
        </Group>
      ) : (
        <Stack gap="xs">
          {findings.map((f, i) => (
            <FindingCard key={`${f.id}-${i}`} f={f} kind={kind} />
          ))}
        </Stack>
      )}
    </Drawer>
  );
}

function FindingCard({ f, kind }: { f: SecurityFinding; kind: SecurityKind }) {
  const scheme = useComputedColorScheme('dark');
  const color = severityColor(f.severity, scheme === 'dark');
  return (
    <Box
      style={{
        border: '1px solid var(--mantine-color-default-border)',
        borderLeft: `3px solid ${color}`,
        borderRadius: 8,
        padding: '10px 12px',
      }}
    >
      <Group justify="space-between" wrap="nowrap" align="flex-start" gap="sm">
        <div style={{ minWidth: 0 }}>
          <Group gap={8} wrap="nowrap">
            <Badge size="sm" variant="filled" color={SEVERITY_META[f.severity].color}>
              {SEVERITY_META[f.severity].label}
            </Badge>
            {f.link ? (
              <Anchor href={f.link} target="_blank" rel="noreferrer" size="sm" fw={700}>
                <Group gap={4} wrap="nowrap">
                  {f.id}
                  <IconExternalLink size={13} />
                </Group>
              </Anchor>
            ) : (
              <Text size="sm" fw={700}>
                {f.id}
              </Text>
            )}
            {typeof f.score === 'number' && f.score > 0 && (
              <Badge size="sm" variant="light" color="gray">
                CVSS {f.score.toFixed(1)}
              </Badge>
            )}
          </Group>
          <Text size="sm" mt={4}>
            {f.title}
          </Text>
        </div>
      </Group>

      {/* Vulnerability: package + version fix. */}
      {kind === 'vulnerability' && f.resource && (
        <Group gap="lg" mt={8}>
          <MetaItem label="Package" value={f.resource} mono />
          {f.installed_version && <MetaItem label="Installed" value={f.installed_version} mono />}
          <MetaItem
            label="Fixed in"
            value={f.fixed_version || 'no fix available'}
            mono={!!f.fixed_version}
            highlight={!!f.fixed_version}
          />
        </Group>
      )}

      {/* Exposed secret: file + redacted match. */}
      {kind === 'exposedsecret' && (
        <Group gap="lg" mt={8}>
          {f.category && <MetaItem label="Category" value={f.category} />}
          {f.target && <MetaItem label="File" value={f.target} mono />}
          {f.match && <MetaItem label="Match" value={f.match} mono />}
        </Group>
      )}

      {/* Config / RBAC check: category, description, remediation. */}
      {(kind === 'configaudit' || kind === 'rbacassessment') && (
        <Stack gap={4} mt={8}>
          {f.category && <MetaItem label="Category" value={f.category} />}
          {f.description && (
            <Text size="xs" c="dimmed">
              {f.description}
            </Text>
          )}
          {f.remediation && (
            <Text size="xs">
              <Text span fw={700} c="teal">
                Fix:{' '}
              </Text>
              {f.remediation}
            </Text>
          )}
        </Stack>
      )}
    </Box>
  );
}

function MetaItem({
  label,
  value,
  mono,
  highlight,
}: {
  label: string;
  value: string;
  mono?: boolean;
  highlight?: boolean;
}) {
  return (
    <div>
      <Text size="10px" c="dimmed" tt="uppercase" fw={700} lh={1.2}>
        {label}
      </Text>
      <Text size="xs" ff={mono ? 'monospace' : undefined} c={highlight ? 'teal' : undefined} fw={highlight ? 700 : 400}>
        {value}
      </Text>
    </div>
  );
}
