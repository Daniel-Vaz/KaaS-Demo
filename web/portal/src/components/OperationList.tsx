import { useState } from 'react';
import {
  ThemeIcon,
  Group,
  Text,
  Stack,
  Badge,
  Loader,
  Tooltip,
  Pagination,
  Modal,
  UnstyledButton,
  Code,
  ScrollArea,
} from '@mantine/core';
import {
  IconCirclePlus,
  IconArrowsUpDown,
  IconPuzzle,
  IconArrowUpCircle,
  IconHistory,
  IconTerminal2,
  IconChevronRight,
  IconServer2,
  IconFirstAidKit,
  IconRestore,
  IconArchive,
  IconCertificate,
  IconDatabaseCog,
  type Icon,
} from '@tabler/icons-react';
import { relative, duration, secondsBetween } from '../lib/format';
import type { Operation, OperationKind } from '../lib/types';

// One shared mapping from an operation kind to its icon + colour, so the Activity log and the
// Upgrades history read the same way. The kinds below the divider are PLATFORM-initiated - the
// automated maintenance and repair the reconciler records on its own - and lean toward warmer,
// health-themed colours so an automated action reads distinctly from a user's edit at a glance.
const KIND_META: Record<OperationKind, { icon: Icon; color: string }> = {
  create: { icon: IconCirclePlus, color: 'grape' },
  scale: { icon: IconArrowsUpDown, color: 'blue' },
  addons: { icon: IconPuzzle, color: 'cyan' },
  upgrade: { icon: IconArrowUpCircle, color: 'teal' },
  disks: { icon: IconServer2, color: 'blue' },
  ssh: { icon: IconTerminal2, color: 'gray' },
  // platform-initiated
  repair: { icon: IconFirstAidKit, color: 'orange' },
  restore: { icon: IconRestore, color: 'red' },
  snapshot: { icon: IconArchive, color: 'indigo' },
  'cert-renewal': { icon: IconCertificate, color: 'lime' },
  defrag: { icon: IconDatabaseCog, color: 'violet' },
};

// A best-effort truncation note the server appends when a session issued more commands than were
// recorded; rendered as a dimmed footnote in the drill-in rather than as a command.
const TRUNCATION_PREFIX = '…';

// SshCommands renders the drill-in for an SSH session operation: a "N commands" button that opens a
// modal listing the commands typed during the session (reconstructed best-effort server-side). It
// replaces the plain inline detail for ssh ops, since a command list is a poor fit for one dimmed line.
function SshCommands({ op }: { op: Operation }) {
  const [open, setOpen] = useState(false);
  const lines = (op.detail ?? '').split('\n').map((l) => l.trim()).filter(Boolean);
  const commands = lines.filter((l) => !l.startsWith(TRUNCATION_PREFIX));
  const note = lines.find((l) => l.startsWith(TRUNCATION_PREFIX));

  if (op.status === 'in_progress') {
    return (
      <Text size="xs" c="dimmed">
        session in progress - commands are recorded when it ends
      </Text>
    );
  }
  if (commands.length === 0) {
    return (
      <Text size="xs" c="dimmed">
        no commands recorded
      </Text>
    );
  }

  return (
    <>
      <UnstyledButton onClick={() => setOpen(true)}>
        <Group gap={4} wrap="nowrap">
          <Text size="xs" c="blue" fw={500}>
            {commands.length} command{commands.length === 1 ? '' : 's'}
          </Text>
          <IconChevronRight size={12} style={{ color: 'var(--mantine-color-blue-5)' }} />
        </Group>
      </UnstyledButton>
      <Modal
        opened={open}
        onClose={() => setOpen(false)}
        size="lg"
        title={
          <Group gap={8}>
            <ThemeIcon variant="light" color="gray" radius="sm" size="md">
              <IconTerminal2 size={16} />
            </ThemeIcon>
            <Text fw={600}>{op.summary}</Text>
          </Group>
        }
      >
        <Text size="xs" c="dimmed" mb="sm">
          Commands typed during the session
          {op.actor_username ? ` by ${op.actor_username}` : ''}, reconstructed from the terminal
          input (best-effort - tab-completions and history recalls may not appear, and output is not
          captured).
        </Text>
        <ScrollArea.Autosize mah={440}>
          <Stack gap={4}>
            {commands.map((cmd, i) => (
              <Group key={i} gap="sm" wrap="nowrap" align="flex-start">
                <Text size="xs" c="dimmed" ff="monospace" w={28} ta="right" style={{ userSelect: 'none' }}>
                  {i + 1}
                </Text>
                <Code style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{cmd}</Code>
              </Group>
            ))}
            {note && (
              <Text size="xs" c="dimmed" fs="italic" mt={4} pl={40}>
                {note}
              </Text>
            )}
          </Stack>
        </ScrollArea.Autosize>
      </Modal>
    </>
  );
}

function OperationRow({ op }: { op: Operation }) {
  const meta = KIND_META[op.kind] ?? { icon: IconHistory, color: 'gray' };
  const Ico = meta.icon;
  const inProgress = op.status === 'in_progress';
  const isSsh = op.kind === 'ssh';
  return (
    <Group wrap="nowrap" align="flex-start" gap="sm">
      <ThemeIcon variant="light" color={meta.color} radius="xl" size={30} mt={2}>
        <Ico size={16} />
      </ThemeIcon>
      <div style={{ flex: 1, minWidth: 0 }}>
        <Group gap="xs" wrap="nowrap" justify="space-between">
          <Text size="sm" fw={500}>
            {op.summary}
          </Text>
          {inProgress ? (
            <Badge size="xs" variant="dot" color="cyan" leftSection={<Loader size={8} color="cyan" />}>
              {isSsh ? 'connected' : 'in progress'}
            </Badge>
          ) : (
            <Tooltip
              label={op.finished_at ? `took ${duration(secondsBetween(op.started_at, op.finished_at))}` : ''}
              disabled={!op.finished_at}
            >
              <Badge size="xs" variant="light" color="teal">
                {isSsh ? 'ended' : 'done'}
              </Badge>
            </Tooltip>
          )}
        </Group>
        {/* ssh ops carry the typed-command list in detail; render it as a drill-in, not inline. */}
        {isSsh ? (
          <SshCommands op={op} />
        ) : (
          op.detail && (
            <Text size="xs" c="dimmed">
              {op.detail}
            </Text>
          )
        )}
        <Text size="xs" c="dimmed">
          {op.actor_username && `by ${op.actor_username} · `}
          {relative(op.started_at)}
          {op.status === 'completed' && op.finished_at
            ? ` · ${isSsh ? 'lasted' : 'completed in'} ${duration(secondsBetween(op.started_at, op.finished_at))}`
            : ''}
        </Text>
      </div>
    </Group>
  );
}

// OperationList renders a cluster's action history newest-first. `kinds` optionally filters to a
// subset (e.g. only 'upgrade' for the Upgrades history). The list is paginated once it grows past
// `pageSize` so history doesn't grow the page without bound.
export function OperationList({
  operations,
  kinds,
  pageSize = 5,
  emptyLabel = 'No operations recorded yet.',
}: {
  operations: Operation[];
  kinds?: OperationKind[];
  pageSize?: number;
  emptyLabel?: string;
}) {
  const [page, setPage] = useState(1);

  let ops = operations;
  if (kinds) ops = ops.filter((o) => kinds.includes(o.kind));

  if (ops.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {emptyLabel}
      </Text>
    );
  }

  const pageCount = Math.ceil(ops.length / pageSize);
  // Clamp: the list polls, so its length can shrink under a stale page number.
  const current = Math.min(Math.max(page, 1), pageCount);
  const shown = ops.slice((current - 1) * pageSize, current * pageSize);

  return (
    <Stack gap="md">
      {shown.map((op) => (
        <OperationRow key={op.id} op={op} />
      ))}
      {pageCount > 1 && (
        <Group justify="space-between" mt="xs">
          <Text size="xs" c="dimmed">
            {ops.length} total
          </Text>
          <Pagination total={pageCount} value={current} onChange={setPage} size="sm" />
        </Group>
      )}
    </Stack>
  );
}
