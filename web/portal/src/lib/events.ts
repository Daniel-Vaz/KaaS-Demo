// useClusterEvents subscribes to the per-cluster SSE provisioning stream and accumulates the
// timeline. The Go broker replays history on connect, so a fresh subscription yields the full
// log. EventSource reconnects on its own; we also re-open when the cluster id changes.

import { useEffect, useRef, useState } from 'react';
import { api } from './api';
import type { ClusterEvent } from './types';

const MAX_EVENTS = 500;

export type StreamStatus = 'connecting' | 'open' | 'closed';

export function useClusterEvents(id: string | undefined) {
  const [events, setEvents] = useState<ClusterEvent[]>([]);
  const [status, setStatus] = useState<StreamStatus>('connecting');
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    setEvents([]);
    if (!id) {
      setStatus('closed');
      return;
    }
    setStatus('connecting');
    const es = new EventSource(api.eventsUrl(id));
    esRef.current = es;

    es.onopen = () => setStatus('open');
    es.onmessage = (ev) => {
      let e: ClusterEvent;
      try {
        e = JSON.parse(ev.data);
      } catch {
        return;
      }
      setEvents((prev) => {
        const next = prev.length >= MAX_EVENTS ? prev.slice(prev.length - MAX_EVENTS + 1) : prev;
        return [...next, e];
      });
    };
    es.onerror = () => setStatus('connecting'); // browser auto-retries

    return () => {
      es.close();
      esRef.current = null;
      setStatus('closed');
    };
  }, [id]);

  return { events, status };
}
