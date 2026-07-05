// The Audit tab body: a live-tailing feed of the cluster's Kubernetes API-server audit events
// (internal/audit). Stat tiles roll up the current page; a filter bar narrows by verb / free-text
// search / denied-only; the table is one row per event with an expandable detail row. Reads are
// view-scoped and poll on a brisk cadence so the feed reads like `tail -f` on the audit log.

import { memo, useCallback, useMemo, useState } from 'react';
import {
  Table,
  Group,
  Text,
  TextInput,
  Select,
  Switch,
  Skeleton,
  Alert,
  Badge,
  Code,
  Collapse,
  SimpleGrid,
  Stack,
  ActionIcon,
  useComputedColorScheme,
} from '@mantine/core';
import { useDebouncedValue } from '@mantine/hooks';
import {
  IconSearch,
  IconAlertTriangle,
  IconListSearch,
  IconChevronRight,
  IconFolders,
  IconUsers,
  IconShieldX,
} from '@tabler/icons-react';
import type { AuditEvent, AuditQuery } from '../lib/types';
import { useAudit } from '../lib/queries';
import { ApiError } from '../lib/api';
import { relative } from '../lib/format';
import { StatCard } from './StatCard';
import { EmptyState } from './EmptyState';

// VERBS is the exact-match verb filter (backend filters server-side). Biased to the mutations the
// default audit policy keeps; "all" clears the filter.
const VERBS = ['create', 'update', 'patch', 'delete', 'get', 'list', 'watch'];

// verbColor keeps the feed scannable: writes stand out, reads recede.
function verbColor(verb: string): string {
  switch (verb) {
    case 'create':
      return 'teal';
    case 'update':
    case 'patch':
      return 'blue';
    case 'delete':
      return 'red';
    case 'deletecollection':
      return 'red';
    default:
      return 'gray'; // get/list/watch and the rest
  }
}

// codeColor maps an HTTP response class to a badge color.
function codeColor(code: number | undefined): string {
  if (!code) return 'gray';
  if (code >= 500) return 'red';
  if (code === 401 || code === 403) return 'orange';
  if (code === 409 || code === 422 || code === 429) return 'yellow';
  if (code >= 400) return 'gray';
  return 'teal';
}

// resourceLabel renders "kind ns/name" (or a non-resource request's URI) compactly.
function resourceLabel(e: AuditEvent): { primary: string; secondary: string } {
  const r = e.resource;
  if (!r?.resource) {
    return { primary: e.request_uri || '-', secondary: 'non-resource' };
  }
  const kind = r.subresource ? `${r.resource}/${r.subresource}` : r.resource;
  const scope = r.namespace ? `${r.namespace}/${r.name ?? ''}` : (r.name ?? 'cluster-scoped');
  return { primary: kind, secondary: scope };
}

// matchesSearch mirrors the backend's free-text match (audit.Query.matches) so the instant
// client-side pass and the debounced server pass agree on what a term means. term is lower-cased.
function matchesSearch(e: AuditEvent, term: string): boolean {
  const r = e.resource;
  return [e.user, e.verb, r?.resource, r?.name, r?.namespace, e.request_uri].some((f) =>
    f ? f.toLowerCase().includes(term) : false,
  );
}

export function AuditPanel({ clusterId, enabled }: { clusterId: string | undefined; enabled: boolean }) {
  const scheme = useComputedColorScheme('dark');
  const [verb, setVerb] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  const [deniedOnly, setDeniedOnly] = useState(false);
  const [limit, setLimit] = useState('200');
  const [expanded, setExpanded] = useState<string | null>(null);

  // Stable across renders, so memoized rows don't all re-render when one is expanded.
  const toggle = useCallback((auditID: string) => {
    setExpanded((cur) => (cur === auditID ? null : auditID));
  }, []);

  // The search box is DEBOUNCED before it reaches the query key: the term is a server-side filter,
  // and each distinct value costs the backend a fresh multi-megabyte `kubectl logs` tail of every
  // apiserver - so keying on raw keystrokes fires one of those per character typed.
  const [debouncedSearch] = useDebouncedValue(search, 350);

  // The server-side filter set. verb + free-text q + limit go to the API; deniedOnly is applied
  // client-side to the returned page (the API has no denied filter). Memoized so the query key is
  // stable across renders that don't change a filter.
  const params: AuditQuery = useMemo(
    () => ({ limit: Number(limit), verb: verb ?? undefined, q: debouncedSearch.trim() || undefined }),
    [limit, verb, debouncedSearch],
  );

  const { data, isLoading, error } = useAudit(clusterId, params, enabled);

  // The current term is ALSO applied client-side, over the same fields the server matches, so typing
  // narrows the feed instantly instead of waiting on the debounce; the refetch then widens the window
  // back out to the whole tail (the server searches lines the capped page never carried).
  const events = useMemo(() => {
    let e = data?.events ?? [];
    if (deniedOnly) e = e.filter((x) => (x.response_code ?? 0) >= 400);
    const term = search.trim().toLowerCase();
    if (term) e = e.filter((x) => matchesSearch(x, term));
    return e;
  }, [data, deniedOnly, search]);

  const stats = data?.stats;

  return (
    <Stack gap="md">
      <Text size="sm" c="dimmed">
        Every write to this cluster's Kubernetes API server is audited by default and streamed here -
        who did what, to which object, and whether it was allowed. Reads are dropped by the audit policy
        to keep the trail focused on changes.
      </Text>

      <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="md">
        <StatCard icon={IconUsers} label="Actors" value={stats?.users ?? '-'} color="cyan" />
        <StatCard icon={IconFolders} label="Namespaces" value={stats?.namespaces ?? '-'} color="brand" />
        <StatCard
          icon={IconShieldX}
          label="Denied"
          value={stats?.denied ?? '-'}
          color={(stats?.denied ?? 0) > 0 ? 'orange' : 'gray'}
        />
        <StatCard
          icon={IconListSearch}
          label="Top verb"
          value={stats?.by_verb?.[0]?.verb ?? '-'}
          color="grape"
          sub={stats?.by_verb?.[0] ? `${stats.by_verb[0].count} events` : undefined}
        />
      </SimpleGrid>

      <Group gap="sm" wrap="wrap" justify="space-between">
        <Group gap="sm" wrap="wrap">
          <TextInput
            placeholder="Search user, resource, name…"
            leftSection={<IconSearch size={15} />}
            value={search}
            onChange={(e) => setSearch(e.currentTarget.value)}
            size="xs"
            w={260}
          />
          <Select
            size="xs"
            w={140}
            placeholder="All verbs"
            clearable
            value={verb}
            onChange={setVerb}
            data={VERBS.map((v) => ({ value: v, label: v }))}
          />
          <Switch
            size="xs"
            label="Denied only"
            checked={deniedOnly}
            onChange={(e) => setDeniedOnly(e.currentTarget.checked)}
          />
        </Group>
        <Group gap="sm" wrap="nowrap">
          <Select
            size="xs"
            w={110}
            value={limit}
            onChange={(v) => setLimit(v ?? '200')}
            data={['100', '200', '500', '1000']}
            aria-label="Row limit"
          />
          {data && (
            <Text size="xs" c="dimmed">
              {events.length} event{events.length === 1 ? '' : 's'}
              {data.truncated ? ' (capped)' : ''}
            </Text>
          )}
        </Group>
      </Group>

      {error ? (
        <Alert color="red" icon={<IconAlertTriangle size={16} />} title="Could not load audit events">
          {error instanceof ApiError ? error.message : String(error)}
        </Alert>
      ) : isLoading && !data ? (
        <Skeleton height={320} radius="md" />
      ) : (data?.events?.length ?? 0) === 0 ? (
        <EmptyState
          icon={IconListSearch}
          title="No audit events yet"
          description="Nothing has hit this cluster's API server since it became Ready - or it was provisioned before audit logging was enabled. Make a change (e.g. deploy something) and it will appear here."
        />
      ) : events.length === 0 ? (
        <EmptyState
          icon={IconSearch}
          title="No matching events"
          description="Try a different search, verb, or clear the denied-only filter."
        />
      ) : (
        <Table.ScrollContainer minWidth={820}>
          <Table highlightOnHover verticalSpacing="xs" striped={scheme === 'light'} layout="fixed">
            <Table.Thead>
              <Table.Tr>
                <Table.Th w={40} />
                <Table.Th w={90}>Time</Table.Th>
                <Table.Th w={90}>Verb</Table.Th>
                <Table.Th>Resource</Table.Th>
                <Table.Th>Actor</Table.Th>
                <Table.Th w={80}>Result</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {events.map((e) => (
                <AuditRow key={e.audit_id} event={e} open={expanded === e.audit_id} onToggle={toggle} />
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Stack>
  );
}

// AuditRow is memoized, and renders its detail panel ONLY while expanded. Both matter at this table's
// size: the feed runs to 1000 rows and re-renders on every 5s poll and every keystroke, and Mantine's
// Collapse keeps its children mounted - so an always-rendered AuditDetail put a full SimpleGrid of
// key/value pairs in the DOM for every row, visible or not.
const AuditRow = memo(function AuditRow({
  event: e,
  open,
  onToggle,
}: {
  event: AuditEvent;
  open: boolean;
  onToggle: (auditID: string) => void;
}) {
  const res = resourceLabel(e);
  return (
    <>
      <Table.Tr onClick={() => onToggle(e.audit_id)} style={{ cursor: 'pointer' }}>
        <Table.Td>
          <ActionIcon variant="subtle" color="gray" size="sm" aria-label={open ? 'Collapse' : 'Expand'}>
            <IconChevronRight
              size={15}
              style={{ transform: open ? 'rotate(90deg)' : 'none', transition: 'transform 120ms' }}
            />
          </ActionIcon>
        </Table.Td>
        <Table.Td>
          {/* A native title rather than a Tooltip: one Mantine Tooltip per row is a popover instance
              per row, and this table renders up to 1000 of them. */}
          <Text size="xs" c="dimmed" title={new Date(e.timestamp).toLocaleString()}>
            {relative(e.timestamp)}
          </Text>
        </Table.Td>
        <Table.Td>
          <Badge size="sm" variant="light" color={verbColor(e.verb)}>
            {e.verb}
          </Badge>
        </Table.Td>
        <Table.Td>
          <div style={{ minWidth: 0 }}>
            <Text size="sm" fw={600} truncate style={{ fontFamily: 'var(--mantine-font-family-monospace)' }}>
              {res.primary}
            </Text>
            <Text size="xs" c="dimmed" truncate>
              {res.secondary}
            </Text>
          </div>
        </Table.Td>
        <Table.Td>
          <Text size="sm" truncate>
            {e.user}
          </Text>
        </Table.Td>
        <Table.Td>
          <Badge size="sm" variant="light" color={codeColor(e.response_code)}>
            {e.response_code || '-'}
          </Badge>
        </Table.Td>
      </Table.Tr>
      <Table.Tr>
        <Table.Td colSpan={6} p={0} style={{ borderBottom: open ? undefined : 'none' }}>
          <Collapse in={open}>{open && <AuditDetail event={e} />}</Collapse>
        </Table.Td>
      </Table.Tr>
    </>
  );
});

function AuditDetail({ event: e }: { event: AuditEvent }) {
  return (
    <Stack gap={6} px="md" py="sm" bg="var(--mantine-color-default-hover)">
      <SimpleGrid cols={{ base: 1, sm: 2 }} spacing={6}>
        <KV label="Request URI">{e.request_uri || '-'}</KV>
        <KV label="Level">{e.level || '-'}</KV>
        <KV label="Stage">{e.stage || '-'}</KV>
        <KV label="Source IPs">{(e.source_ips ?? []).join(', ') || '-'}</KV>
        <KV label="User agent">{e.user_agent || '-'}</KV>
        <KV label="Audit ID">{e.audit_id}</KV>
      </SimpleGrid>
      {(e.groups?.length ?? 0) > 0 && (
        <Group gap={4} wrap="wrap">
          <Text size="xs" c="dimmed" fw={600}>
            Groups:
          </Text>
          {(e.groups ?? []).map((g) => (
            <Badge key={g} size="xs" variant="outline" color="gray">
              {g}
            </Badge>
          ))}
        </Group>
      )}
    </Stack>
  );
}

function KV({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group gap={8} wrap="nowrap" align="baseline">
      <Text size="xs" c="dimmed" fw={600} w={90} style={{ flexShrink: 0 }}>
        {label}
      </Text>
      <Code style={{ fontSize: 11, wordBreak: 'break-all', background: 'transparent', padding: 0 }}>
        {children}
      </Code>
    </Group>
  );
}
