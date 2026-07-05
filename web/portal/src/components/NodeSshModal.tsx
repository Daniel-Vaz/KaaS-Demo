import { useState } from 'react';
import { ActionIcon, Box, Code, Group, Modal, Text, ThemeIcon, Tooltip } from '@mantine/core';
import {
  IconArrowsMaximize,
  IconArrowsMinimize,
  IconRefresh,
  IconServer,
  IconServerBolt,
} from '@tabler/icons-react';
import { api } from '../lib/api';
import type { Cluster, Node } from '../lib/types';
import { TerminalSession, TerminalStatusBadge, type TerminalStatus } from './TerminalSession';

/**
 * NodeSshModal opens an in-browser SSH session (as the kaas user) to a single cluster node, wired to
 * GET /api/clusters/{id}/nodes/{vm}/ssh. It reuses the shared TerminalSession engine inside a large,
 * fullscreen-toggleable modal so the console gets real estate. The parent gates rendering on write
 * access + a resolved IP; the API is the authoritative gate (a read-role member gets a 403).
 *
 * Two Mantine defaults are deliberately turned OFF: closeOnEscape (Escape is an ordinary terminal
 * key - vim, less, readline - so it must reach the session, not dismiss the modal) and
 * closeOnClickOutside (a drag to select terminal text can release outside the modal). Closing is
 * explicit via the ✕.
 */
export function NodeSshModal({
  cluster,
  node,
  onClose,
}: {
  cluster: Cluster;
  node: Node | null;
  onClose: () => void;
}) {
  const [status, setStatus] = useState<TerminalStatus>('connecting');
  const [reconnectKey, setReconnectKey] = useState(0);
  const [fullScreen, setFullScreen] = useState(false);

  // Follows NodeDetailPane: presence of the node is the open state, and we unmount when it clears -
  // which also resets status/reconnect/fullscreen to fresh state for the next node.
  if (!node) return null;
  const isCP = node.role === 'control-plane';

  return (
    <Modal
      opened
      onClose={onClose}
      fullScreen={fullScreen}
      size="90%"
      radius={fullScreen ? 0 : 'md'}
      closeOnEscape={false}
      closeOnClickOutside={false}
      padding="md"
      overlayProps={{ backgroundOpacity: 0.55, blur: 2 }}
      styles={{ title: { flex: 1 }, body: { paddingTop: 8 } }}
      title={
        <Group justify="space-between" wrap="nowrap" pr="sm">
          <Group gap={8} wrap="nowrap" style={{ minWidth: 0 }}>
            <ThemeIcon variant="light" color={isCP ? 'grape' : 'gray'} radius="sm" size="md">
              {isCP ? <IconServerBolt size={16} /> : <IconServer size={16} />}
            </ThemeIcon>
            <Text ff="monospace" fw={600} truncate>
              kaas@{node.vm_name}
            </Text>
            {node.ip && <Code>{node.ip}</Code>}
            <TerminalStatusBadge status={status} />
          </Group>
          <Group gap={4} wrap="nowrap">
            {(status === 'closed' || status === 'error') && (
              <Tooltip label="Reconnect" withArrow>
                <ActionIcon variant="light" onClick={() => setReconnectKey((k) => k + 1)} aria-label="Reconnect">
                  <IconRefresh size={16} />
                </ActionIcon>
              </Tooltip>
            )}
            <Tooltip label={fullScreen ? 'Exit full screen' : 'Full screen'} withArrow>
              <ActionIcon
                variant="subtle"
                color="gray"
                onClick={() => setFullScreen((v) => !v)}
                aria-label={fullScreen ? 'Exit full screen' : 'Full screen'}
              >
                {fullScreen ? <IconArrowsMinimize size={16} /> : <IconArrowsMaximize size={16} />}
              </ActionIcon>
            </Tooltip>
          </Group>
        </Group>
      }
    >
      <Box
        style={{
          background: '#0d1117',
          borderRadius: 8,
          padding: 8,
          height: fullScreen ? 'calc(100vh - 132px)' : '70vh',
          overflow: 'hidden',
        }}
      >
        <TerminalSession
          url={api.nodeSshUrl(cluster.id, node.vm_name)}
          reconnectSignal={reconnectKey}
          onStatusChange={setStatus}
        />
      </Box>
      <Text size="xs" c="dimmed" mt={6}>
        SSH as <b>kaas</b> (passwordless sudo). Ctrl+Shift+C copies the selection. This session is
        audited on the cluster’s Activity tab.
      </Text>
    </Modal>
  );
}
