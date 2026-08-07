import { useEffect, useRef } from 'react';
import { Badge, useComputedColorScheme } from '@mantine/core';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { copyText } from '../lib/clipboard';

export type TerminalStatus = 'connecting' | 'open' | 'closed' | 'error';

// xterm colour themes tuned to the portal's light/dark schemes. Only the handful of fields that
// matter for a readable console; xterm fills the rest of the 16-colour palette with sane defaults.
const darkTheme = {
  background: '#0b0e14',
  foreground: '#c9d1d9',
  cursor: '#58a6ff',
  cursorAccent: '#0b0e14',
  selectionBackground: '#2d4f7c',
};
const lightTheme = {
  background: '#0d1117', // keep a dark terminal even in light mode - a familiar console look
  foreground: '#c9d1d9',
  cursor: '#58a6ff',
  cursorAccent: '#0d1117',
  selectionBackground: '#2d4f7c',
};

/**
 * TerminalSession is the shared xterm-over-WebSocket engine behind both terminal surfaces - the
 * cluster shell (ClusterShell) and node SSH (NodeSshModal). It owns the terminal DOM, the socket
 * lifecycle, resize propagation, copy-on-Ctrl+Shift+C and reconnect; it is deliberately chrome-free
 * (no header, no status badge, no card) and fills 100% of its parent, so each consumer supplies its
 * own framing and status display via `onStatusChange`. Bump `reconnectSignal` to tear down and
 * redial the same url.
 *
 * Wire protocol (identical for both seams): keystrokes go up as binary frames (stdin); output comes
 * back as binary frames written to xterm; resizes and server exit/error notices ride text (JSON)
 * frames. The backend - in-process fake or a real PTY behind an agent - is indistinguishable here.
 */
export function TerminalSession({
  url,
  reconnectSignal = 0,
  onStatusChange,
}: {
  url: string;
  reconnectSignal?: number;
  onStatusChange?: (status: TerminalStatus) => void;
}) {
  const scheme = useComputedColorScheme('dark');
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  // Hold the latest callback in a ref so changing its identity never re-dials the socket - only url
  // and reconnectSignal do.
  const onStatusRef = useRef(onStatusChange);
  onStatusRef.current = onStatusChange;

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const term = new Terminal({
      convertEol: false, // the backend already emits CRLF
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, "Liberation Mono", monospace',
      scrollback: 2000,
      theme: scheme === 'light' ? lightTheme : darkTheme,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(container);
    termRef.current = term;

    // Ctrl+Shift+C copies the current selection (like a terminal emulator) instead of sending an
    // ETX/SIGINT. Returning false stops xterm from also forwarding the keystroke as stdin. Plain
    // Ctrl+C still reaches the shell. Any other key is handled by xterm as usual.
    term.attachCustomKeyEventHandler((e) => {
      if (e.type === 'keydown' && e.ctrlKey && e.shiftKey && (e.key === 'C' || e.key === 'c')) {
        const selection = term.getSelection();
        if (selection) {
          void copyText(selection);
          return false;
        }
      }
      return true;
    });

    const safeFit = () => {
      try {
        fit.fit();
      } catch {
        /* container not measurable yet */
      }
    };
    safeFit();

    let status: TerminalStatus = 'connecting';
    const setStatus = (next: TerminalStatus) => {
      status = next;
      onStatusRef.current?.(next);
    };
    setStatus('connecting');

    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    const enc = new TextEncoder();

    const sendResize = () => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', rows: term.rows, cols: term.cols }));
      }
    };

    ws.onopen = () => {
      setStatus('open');
      safeFit();
      sendResize();
      term.focus();
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        // Text frame = JSON control message (exit / error) from the server.
        try {
          const msg = JSON.parse(ev.data) as { type: string; code?: number; message?: string };
          if (msg.type === 'error') {
            term.write(`\r\n\x1b[31m${msg.message ?? 'session error'}\x1b[0m\r\n`);
            setStatus('error');
          } else if (msg.type === 'exit') {
            term.write(`\r\n\x1b[90m[session exited${msg.code ? ` (code ${msg.code})` : ''}]\x1b[0m\r\n`);
          }
        } catch {
          /* ignore malformed control frame */
        }
        return;
      }
      term.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    ws.onerror = () => {
      if (status !== 'open') setStatus('error');
    };
    ws.onclose = () => {
      if (status !== 'error') setStatus('closed');
    };

    const onData = term.onData((d) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d));
    });
    const onResize = term.onResize(() => sendResize());

    // Refit (and thus resize the PTY) whenever the container changes size.
    const ro = new ResizeObserver(() => safeFit());
    ro.observe(container);

    return () => {
      ro.disconnect();
      onData.dispose();
      onResize.dispose();
      ws.onclose = null; // avoid a state update after unmount
      ws.close();
      term.dispose();
      termRef.current = null;
    };
  }, [url, reconnectSignal]); // eslint-disable-line react-hooks/exhaustive-deps -- scheme handled separately

  // Recolour the live terminal when the user toggles light/dark, without tearing down the session.
  useEffect(() => {
    if (termRef.current) {
      termRef.current.options.theme = scheme === 'light' ? lightTheme : darkTheme;
    }
  }, [scheme]);

  return <div ref={containerRef} style={{ width: '100%', height: '100%' }} />;
}

// TerminalStatusBadge renders a connection-state dot, shared by both terminal surfaces' headers.
export function TerminalStatusBadge({ status }: { status: TerminalStatus }) {
  const map = {
    connecting: { color: 'yellow', label: 'connecting' },
    open: { color: 'teal', label: 'connected' },
    closed: { color: 'gray', label: 'disconnected' },
    error: { color: 'red', label: 'error' },
  }[status];
  return (
    <Badge color={map.color} variant="dot" size="sm">
      {map.label}
    </Badge>
  );
}
