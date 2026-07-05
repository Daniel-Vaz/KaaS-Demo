import { useEffect, useRef } from 'react';
import { Badge, Box, Group, Text, ScrollArea, Center, Loader } from '@mantine/core';
import { IconActivity } from '@tabler/icons-react';
import { timeOfDay } from '../lib/format';
import type { ClusterEvent } from '../lib/types';
import type { StreamStatus } from '../lib/events';
import classes from './ActivityTimeline.module.css';

const SOURCE_COLOR: Record<string, string> = {
  infra: 'blue',
  ansible: 'grape',
  addon: 'cyan',
  reconciler: 'gray',
};

function StatusDot({ status }: { status: StreamStatus }) {
  if (status === 'open') {
    return (
      <Badge color="teal" variant="dot" size="sm">
        live
      </Badge>
    );
  }
  if (status === 'connecting') {
    return (
      <Badge color="yellow" variant="dot" size="sm">
        connecting
      </Badge>
    );
  }
  return (
    <Badge color="gray" variant="dot" size="sm">
      closed
    </Badge>
  );
}

export function ActivityTimeline({
  events,
  status,
  height = 420,
}: {
  events: ClusterEvent[];
  status: StreamStatus;
  height?: number;
}) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const pinnedRef = useRef(true);

  // Autoscroll only when the user is already near the bottom, so scrolling up to read history
  // isn't yanked back down by new lines.
  useEffect(() => {
    const vp = viewportRef.current;
    if (vp && pinnedRef.current) {
      vp.scrollTo({ top: vp.scrollHeight });
    }
  }, [events]);

  return (
    <Box>
      <Group justify="space-between" mb="xs">
        <Group gap={6}>
          <IconActivity size={16} />
          <Text size="sm" fw={600}>
            Live activity
          </Text>
        </Group>
        <StatusDot status={status} />
      </Group>
      <ScrollArea
        h={height}
        viewportRef={viewportRef}
        onScrollPositionChange={({ y }) => {
          const vp = viewportRef.current;
          if (!vp) return;
          pinnedRef.current = y + vp.clientHeight >= vp.scrollHeight - 24;
        }}
        className={classes.log}
      >
        {events.length === 0 ? (
          <Center h={height - 24}>
            <Group gap="xs" c="dimmed">
              <Loader size="xs" />
              <Text size="sm">Waiting for provisioning events…</Text>
            </Group>
          </Center>
        ) : (
          events.map((e, i) => (
            <div key={i} className={classes.line}>
              <span className={classes.ts}>{timeOfDay(e.ts)}</span>
              <Badge
                size="xs"
                radius="sm"
                variant="light"
                color={SOURCE_COLOR[e.source] ?? 'gray'}
                className={classes.src}
              >
                {e.source || '-'}
              </Badge>
              <span className={e.level === 'error' ? classes.error : e.level === 'warn' ? classes.warn : classes.msg}>
                {e.message}
              </span>
            </div>
          ))
        )}
      </ScrollArea>
    </Box>
  );
}
