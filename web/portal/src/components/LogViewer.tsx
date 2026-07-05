import { useEffect, useMemo, useRef, useState } from 'react';
import { Group, Select, Switch, Badge, Button, Box, Text } from '@mantine/core';
import { IconRefresh } from '@tabler/icons-react';
import { api, type WorkloadRef } from '../lib/api';
import type { PodInfo } from '../lib/types';

type Status = 'connecting' | 'open' | 'closed' | 'error';

/**
 * LogViewer streams a pod/container's logs from GET /api/clusters/{id}/workloads/.../logs over a
 * WebSocket (kubectl logs [-f], fake-synthesized or worker-proxied). Log bytes arrive as binary
 * frames and are appended imperatively to a scroll pane (so a fast stream doesn't trigger a React
 * re-render per line); a text frame is a JSON control message (error). Mirrors ClusterShell's socket
 * handling. Changing pod/container/follow, or Reconnect, tears down and reopens the stream.
 */
export function LogViewer({
  clusterId,
  workloadRef,
  pods,
}: {
  clusterId: string;
  workloadRef: WorkloadRef;
  pods: PodInfo[];
}) {
  const paneRef = useRef<HTMLDivElement>(null);
  const [pod, setPod] = useState<string>(pods[0]?.name ?? '');
  const [container, setContainer] = useState<string>('');
  const [follow, setFollow] = useState(true);
  const [tail, setTail] = useState('200');
  const [status, setStatus] = useState<Status>('connecting');
  const [reconnectKey, setReconnectKey] = useState(0);

  // Keep the selected pod valid as the pod list refreshes; default the container to the pod's first.
  const currentPod = useMemo(() => pods.find((p) => p.name === pod) ?? pods[0], [pods, pod]);
  const containers = currentPod?.containers ?? [];

  useEffect(() => {
    if (pods.length && !pods.some((p) => p.name === pod)) setPod(pods[0].name);
  }, [pods, pod]);

  useEffect(() => {
    // Reset the container selection when the pod changes and the old container is gone.
    if (container && !containers.includes(container)) setContainer('');
  }, [containers, container]);

  const refKey = `${workloadRef.kind}/${workloadRef.namespace}/${workloadRef.name}`;

  useEffect(() => {
    const pane = paneRef.current;
    if (!pane || !currentPod) return;
    pane.textContent = '';

    setStatus('connecting');
    const url = api.workloadLogsUrl(clusterId, workloadRef, {
      pod: currentPod.name,
      container: container || undefined,
      tail: Number(tail),
      follow,
    });
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    const decoder = new TextDecoder();

    const append = (text: string) => {
      const atBottom = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 40;
      pane.appendChild(document.createTextNode(text));
      if (follow || atBottom) pane.scrollTop = pane.scrollHeight;
    };

    ws.onopen = () => setStatus('open');
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        try {
          const msg = JSON.parse(ev.data) as { type?: string; message?: string };
          if (msg.type === 'error') {
            append(`\n[error] ${msg.message ?? 'log stream error'}\n`);
            setStatus('error');
          }
        } catch {
          /* ignore malformed control frame */
        }
        return;
      }
      append(decoder.decode(new Uint8Array(ev.data as ArrayBuffer)));
    };
    ws.onerror = () => setStatus((s) => (s === 'open' ? s : 'error'));
    ws.onclose = () => setStatus((s) => (s === 'error' ? s : 'closed'));

    return () => {
      ws.onclose = null;
      ws.close();
    };
  }, [clusterId, refKey, currentPod?.name, container, follow, tail, reconnectKey]); // eslint-disable-line react-hooks/exhaustive-deps

  if (pods.length === 0) {
    return (
      <Text c="dimmed" size="sm" py="md">
        This workload has no running pods to stream logs from.
      </Text>
    );
  }

  return (
    <Box>
      <Group justify="space-between" mb="xs" wrap="wrap" gap="sm">
        <Group gap="sm" wrap="wrap">
          <Select
            size="xs"
            label="Pod"
            data={pods.map((p) => ({ value: p.name, label: p.name }))}
            value={currentPod?.name ?? null}
            onChange={(v) => v && setPod(v)}
            searchable
            w={260}
          />
          <Select
            size="xs"
            label="Container"
            data={
              containers.length > 1
                ? [
                    { value: '', label: 'Default (first)' },
                    ...containers.map((c) => ({ value: c, label: c })),
                  ]
                : [{ value: '', label: containers[0] ?? 'default' }]
            }
            value={container}
            onChange={(v) => setContainer(v ?? '')}
            w={180}
          />
          <Select
            size="xs"
            label="Tail"
            data={['100', '200', '500', '1000'].map((n) => ({ value: n, label: `${n} lines` }))}
            value={tail}
            onChange={(v) => v && setTail(v)}
            w={130}
          />
        </Group>
        <Group gap="sm" align="flex-end">
          <Switch
            label="Follow"
            size="sm"
            checked={follow}
            onChange={(e) => setFollow(e.currentTarget.checked)}
          />
          <StatusBadge status={status} />
          <Button
            size="compact-xs"
            variant="light"
            leftSection={<IconRefresh size={13} />}
            onClick={() => setReconnectKey((k) => k + 1)}
          >
            Reconnect
          </Button>
        </Group>
      </Group>
      <Box
        ref={paneRef}
        style={{
          background: '#0d1117',
          color: '#c9d1d9',
          borderRadius: 8,
          padding: 12,
          height: 440,
          overflow: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
          fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, "Liberation Mono", monospace',
          fontSize: 12.5,
          lineHeight: 1.5,
        }}
      />
    </Box>
  );
}

function StatusBadge({ status }: { status: Status }) {
  const map = {
    connecting: { color: 'yellow', label: 'connecting' },
    open: { color: 'teal', label: 'streaming' },
    closed: { color: 'gray', label: 'ended' },
    error: { color: 'red', label: 'error' },
  }[status];
  return (
    <Badge color={map.color} variant="dot" size="sm">
      {map.label}
    </Badge>
  );
}
