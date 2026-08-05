// The wasm module's side of the demo bridge - the exact surface cmd/demo-wasm/bridge.go installs on
// globalThis. Nothing else in the portal may touch `__kaas`; shim.ts is the only consumer, and it
// exists to make the rest of the portal unable to tell that it is talking to a module rather than a
// server.

export interface DemoRequest {
  method: string;
  url: string;
  headers: Record<string, string>;
  body?: string;
}

export interface DemoResponseCallbacks {
  onHead: (status: number, headers: Record<string, string>) => void;
  onChunk: (chunk: Uint8Array) => void;
  onEnd: (err: string | null) => void;
}

export interface DemoTerminalCallbacks {
  onOpen: () => void;
  onBinary: (chunk: Uint8Array) => void;
  onText: (text: string) => void;
  onClose: (code: number, reason: string) => void;
}

export interface DemoTerminal {
  send: (data: Uint8Array) => void;
  sendText: (text: string) => void;
  close: () => void;
}

export interface DemoBridge {
  /** Drives one request through the API handler. Returns an abort function. */
  fetch: (req: DemoRequest, cbs: DemoResponseCallbacks) => () => void;
  /** Opens one terminal session (cluster shell, node SSH or pod logs - the URL decides). */
  terminal: (req: DemoRequest, cbs: DemoTerminalCallbacks) => DemoTerminal;
}

declare global {
  // eslint-disable-next-line no-var
  var __kaas: DemoBridge | undefined;
  // eslint-disable-next-line no-var
  var __kaasBoot: ((state: 'ready' | 'error', detail: string | null) => void) | undefined;
  // wasm_exec.js installs this.
  // eslint-disable-next-line no-var
  var Go: { new (): { importObject: WebAssembly.Imports; env: Record<string, string>; run: (i: WebAssembly.Instance) => void } };
}

export function bridge(): DemoBridge {
  if (!globalThis.__kaas) throw new Error('the demo control plane is not running');
  return globalThis.__kaas;
}
