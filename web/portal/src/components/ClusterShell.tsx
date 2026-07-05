import { useState } from 'react';
import { Badge, Box, Button, Group, Text } from '@mantine/core';
import { IconRefresh, IconTerminal2 } from '@tabler/icons-react';
import { api } from '../lib/api';
import { TerminalSession, TerminalStatusBadge, type TerminalStatus } from './TerminalSession';

/**
 * ClusterShell renders an interactive kubectl terminal wired to GET /api/clusters/{id}/shell. It is
 * the inline Terminal-tab surface: a titled header (with a read-only badge, live status and a
 * reconnect button) over a fixed-height dark console. All the xterm/WebSocket machinery lives in the
 * shared TerminalSession - this component is only the chrome.
 */
export function ClusterShell({ id, readOnly = false }: { id: string; readOnly?: boolean }) {
  const [status, setStatus] = useState<TerminalStatus>('connecting');
  const [reconnectKey, setReconnectKey] = useState(0);

  return (
    <Box>
      <Group justify="space-between" mb="xs">
        <Group gap={6}>
          <IconTerminal2 size={16} />
          <Text size="sm" fw={600}>
            kubectl shell
          </Text>
          {readOnly && (
            <Badge color="gray" variant="light" size="sm">
              read-only
            </Badge>
          )}
        </Group>
        <Group gap="xs">
          <TerminalStatusBadge status={status} />
          {(status === 'closed' || status === 'error') && (
            <Button
              size="compact-xs"
              variant="light"
              leftSection={<IconRefresh size={13} />}
              onClick={() => setReconnectKey((k) => k + 1)}
            >
              Reconnect
            </Button>
          )}
        </Group>
      </Group>
      <Box
        style={{
          background: '#0d1117',
          borderRadius: 8,
          padding: 8,
          height: 460,
          overflow: 'hidden',
        }}
      >
        <TerminalSession url={api.shellUrl(id)} reconnectSignal={reconnectKey} onStatusChange={setStatus} />
      </Box>
    </Box>
  );
}
