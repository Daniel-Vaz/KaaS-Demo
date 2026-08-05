// The demo shim: it makes the in-page wasm control plane look like a server to the rest of the
// portal.
//
// Three browser APIs carry every byte the portal exchanges with the API - fetch (all JSON and YAML),
// EventSource (the per-cluster provisioning feed) and WebSocket (the terminals and live pod logs) -
// so patching exactly those three is enough, and NOTHING under src/lib or src/pages needs to know
// this build exists. Requests that are not the API's (assets, Vite's HMR socket) are handed straight
// to the original implementation.
//
// One thing genuinely cannot work the way it does against a server: cookies. A browser ignores
// Set-Cookie on a Response that JavaScript constructed, so the session cookie is kept here and
// re-attached to every request as an ordinary header. The Go side is unchanged and unaware - it
// reads the same `Cookie:` header it always did.

import { bridge, type DemoRequest } from './bridge';

/** Requests whose path starts with this are the API's. Everything else passes through. */
const API_PREFIX = '/api';

/** The session cookie, held here because the browser will not hold it for us. */
let cookieJar = '';

const SESSION_COOKIE = 'kaas_session';

const SAME_ORIGIN_SCHEMES = ['http:', 'https:', 'ws:', 'wss:'];

function apiPath(url: string): string | null {
  let u: URL;
  try {
    u = new URL(url, window.location.href);
  } catch {
    return null;
  }
  if (!SAME_ORIGIN_SCHEMES.includes(u.protocol)) return null;
  // Compared on HOST, not origin: a ws: URL's origin is "ws://host", which never equals the page's
  // "http://host" however same-origin the two are. The portal builds its terminal URLs that way
  // (api.shellUrl), so an origin comparison here silently sends every terminal to the network.
  if (u.host !== window.location.host) return null;
  if (u.pathname !== API_PREFIX && !u.pathname.startsWith(API_PREFIX + '/')) return null;
  // The API's own routes live at the root (/clusters, /catalog, …); `/api` is the prefix nginx and
  // the Vite dev proxy strip, so we strip it too.
  return u.pathname.slice(API_PREFIX.length) + u.search;
}

function toRequest(method: string, path: string, headers: Record<string, string> = {}, body?: string): DemoRequest {
  // The URL only has to be absolute for http.NewRequest; the host is never used.
  return { method, url: 'http://demo' + path, headers: { ...headers, ...(cookieJar ? { Cookie: cookieJar } : {}) }, body };
}

/**
 * Records the session cookie the API just issued, or forgets it on logout. Only the session cookie
 * is tracked: it is the only one the API sets, and a jar that accepted anything would be a cookie
 * implementation, which this is deliberately not.
 */
function absorbSetCookie(headers: Record<string, string>) {
  const raw = headers['Set-Cookie'] ?? headers['set-cookie'];
  if (!raw) return;
  const [pair, ...attrs] = raw.split(';').map((s) => s.trim());
  if (!pair.startsWith(SESSION_COOKIE + '=')) return;
  // Logout expires the cookie with a negative Max-Age rather than clearing the value.
  const expired = attrs.some((a) => {
    const m = /^max-age=(-?\d+)$/i.exec(a);
    return m !== null && Number(m[1]) <= 0;
  });
  cookieJar = expired || pair === SESSION_COOKIE + '=' ? '' : pair;
}

// --- fetch -------------------------------------------------------------------

function installFetch() {
  const realFetch = window.fetch.bind(window);

  window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const path = apiPath(url);
    if (path === null) return realFetch(input as RequestInfo, init);

    const method = (init?.method ?? (typeof input === 'object' && 'method' in input ? input.method : 'GET')).toUpperCase();
    const headers: Record<string, string> = {};
    new Headers(init?.headers ?? (typeof input === 'object' && 'headers' in input ? input.headers : undefined)).forEach(
      (v, k) => {
        headers[k] = v;
      },
    );
    const body = typeof init?.body === 'string' ? init.body : undefined;

    return new Promise<Response>((resolve, reject) => {
      let settled = false;
      let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
      const pending: Uint8Array[] = [];
      let ended: string | null | undefined;

      const stream = new ReadableStream<Uint8Array>({
        start(c) {
          controller = c;
          pending.forEach((p) => c.enqueue(p));
          pending.length = 0;
          if (ended !== undefined) finish(c, ended);
        },
        // The portal cancels a stream when it navigates away from an event feed; that has to reach
        // the Go side, whose SSE handler blocks until its request context is done.
        cancel() {
          abort();
        },
      });

      const finish = (c: ReadableStreamDefaultController<Uint8Array>, err: string | null) => {
        try {
          if (err) c.error(new Error(err));
          else c.close();
        } catch {
          /* already closed by a cancel() */
        }
      };

      const abort = bridge().fetch(toRequest(method, path, headers, body), {
        onHead: (status, respHeaders) => {
          absorbSetCookie(respHeaders);
          settled = true;
          // 204/205/304 must not carry a body, and the Response constructor enforces it.
          const bodyless = status === 204 || status === 205 || status === 304;
          resolve(new Response(bodyless ? null : stream, { status, headers: respHeaders }));
        },
        onChunk: (chunk) => {
          // The chunk is a view onto wasm memory that Go may reuse; copy before it leaves.
          const copy = new Uint8Array(chunk);
          if (controller) controller.enqueue(copy);
          else pending.push(copy);
        },
        onEnd: (err) => {
          if (!settled) {
            reject(new Error(err ?? 'the demo control plane closed the request'));
            return;
          }
          if (controller) finish(controller, err);
          else ended = err;
        },
      });
    });
  };
}

// --- EventSource -------------------------------------------------------------

/**
 * A minimal EventSource over the bridge, covering what the portal uses: onopen/onmessage/onerror,
 * addEventListener, close() and readyState. It deliberately does NOT auto-reconnect the way a real
 * EventSource does - there is no network to drop, so a stream that ends has ended.
 *
 * It does not say `implements EventSource`: the DOM's typed addEventListener overloads are not
 * satisfiable by a subclass of EventTarget, so the declaration would only be a cast in disguise.
 */
class DemoEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readonly CONNECTING = 0 as const;
  readonly OPEN = 1 as const;
  readonly CLOSED = 2 as const;

  readonly url: string;
  readonly withCredentials = false;
  readyState: number = DemoEventSource.CONNECTING;

  onopen: ((this: EventSource, ev: Event) => unknown) | null = null;
  onmessage: ((this: EventSource, ev: MessageEvent) => unknown) | null = null;
  onerror: ((this: EventSource, ev: Event) => unknown) | null = null;

  #abort: (() => void) | null = null;
  #closed = false;
  #buf = '';
  #decoder = new TextDecoder();

  constructor(url: string) {
    super();
    this.url = new URL(url, window.location.href).href;
    const path = apiPath(url) ?? '/';

    // Deferred by a microtask, and this is load-bearing. Go's wasm scheduler runs a newly spawned
    // goroutine before returning control to the JS caller, so a bridge call made here would deliver
    // its first callback DURING this constructor - before the caller has assigned onopen/onmessage,
    // which every caller does on the line after `new`. Neither a real EventSource nor a real
    // WebSocket can fire that early; deferring restores the same guarantee.
    queueMicrotask(() => {
      if (this.#closed) return;
      this.#abort = bridge().fetch(toRequest('GET', path), {
        onHead: (status) => {
          if (status !== 200) {
            this.#fail();
            return;
          }
          this.readyState = DemoEventSource.OPEN;
          this.#emit('open', new Event('open'));
        },
        onChunk: (chunk) => this.#feed(this.#decoder.decode(chunk, { stream: true })),
        onEnd: () => this.#fail(),
      });
    });
  }

  // Frames are "data: <json>\n\n". Only the default `message` event is ever sent by the API, so
  // there is no event-name parsing to do.
  #feed(text: string) {
    this.#buf += text;
    let split: number;
    while ((split = this.#buf.indexOf('\n\n')) !== -1) {
      const frame = this.#buf.slice(0, split);
      this.#buf = this.#buf.slice(split + 2);
      const data = frame
        .split('\n')
        .filter((l) => l.startsWith('data:'))
        .map((l) => l.slice(5).trimStart())
        .join('\n');
      if (data) this.#emit('message', new MessageEvent('message', { data }));
    }
  }

  #fail() {
    if (this.readyState === DemoEventSource.CLOSED) return;
    this.readyState = DemoEventSource.CLOSED;
    this.#emit('error', new Event('error'));
  }

  #emit(kind: 'open' | 'message' | 'error', ev: Event) {
    const handler = kind === 'open' ? this.onopen : kind === 'message' ? this.onmessage : this.onerror;
    (handler as ((ev: Event) => unknown) | null)?.call(this, ev);
    this.dispatchEvent(ev);
  }

  close() {
    this.readyState = DemoEventSource.CLOSED;
    this.#closed = true;
    this.#abort?.();
  }
}

// --- WebSocket ---------------------------------------------------------------

/**
 * A WebSocket over the bridge, covering what TerminalSession and LogViewer use: binaryType,
 * readyState, send() of a string or Uint8Array, close(), and the four on* handlers. Binary frames
 * arrive as an ArrayBuffer, matching `binaryType = 'arraybuffer'`, which both callers set.
 *
 * Not declared `implements WebSocket`, for the same reason as DemoEventSource above.
 */
class DemoWebSocket extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly CONNECTING = 0 as const;
  readonly OPEN = 1 as const;
  readonly CLOSING = 2 as const;
  readonly CLOSED = 3 as const;

  readonly url: string;
  readonly protocol = '';
  readonly extensions = '';
  readonly bufferedAmount = 0;
  binaryType: BinaryType = 'blob';
  readyState: number = DemoWebSocket.CONNECTING;

  onopen: ((this: WebSocket, ev: Event) => unknown) | null = null;
  onmessage: ((this: WebSocket, ev: MessageEvent) => unknown) | null = null;
  onerror: ((this: WebSocket, ev: Event) => unknown) | null = null;
  onclose: ((this: WebSocket, ev: CloseEvent) => unknown) | null = null;

  #term: ReturnType<ReturnType<typeof bridge>['terminal']> | null = null;
  #closed = false;
  #encoder = new TextEncoder();

  constructor(url: string | URL) {
    super();
    const href = typeof url === 'string' ? url : url.href;
    this.url = href;
    const path = apiPath(href) ?? '/';

    // Deferred for the reason spelled out on DemoEventSource: the bridge would otherwise call back
    // before the caller has attached its handlers.
    queueMicrotask(() => {
      if (this.#closed) return;
      this.#term = bridge().terminal(toRequest('GET', path), {
        onOpen: () => {
          this.readyState = DemoWebSocket.OPEN;
          this.#emit('open', new Event('open'));
        },
        onBinary: (chunk) => {
          // Copy out of wasm memory, then present it the way the requested binaryType says.
          const copy = new Uint8Array(chunk);
          const data = this.binaryType === 'blob' ? new Blob([copy]) : copy.buffer;
          this.#emit('message', new MessageEvent('message', { data }));
        },
        onText: (text) => this.#emit('message', new MessageEvent('message', { data: text })),
        onClose: (code, reason) => {
          if (this.readyState === DemoWebSocket.CLOSED) return;
          this.readyState = DemoWebSocket.CLOSED;
          this.#emit('close', new CloseEvent('close', { code, reason }));
        },
      });
    });
  }

  send(data: string | ArrayBufferLike | Blob | ArrayBufferView) {
    if (this.readyState !== DemoWebSocket.OPEN || !this.#term) return;
    if (typeof data === 'string') {
      // A text frame is a JSON control message (resize); the wire protocol splits on frame type.
      this.#term.sendText(data);
    } else if (ArrayBuffer.isView(data)) {
      this.#term.send(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
    } else if (data instanceof ArrayBuffer) {
      this.#term.send(new Uint8Array(data));
    } else {
      // A Blob would need an async read; nothing in the portal sends one.
      this.#term.send(this.#encoder.encode(String(data)));
    }
  }

  close() {
    if (this.readyState === DemoWebSocket.CLOSED) return;
    this.readyState = DemoWebSocket.CLOSING;
    this.#closed = true;
    this.#term?.close();
  }

  #emit(kind: 'open' | 'message' | 'error' | 'close', ev: Event) {
    const handler =
      kind === 'open' ? this.onopen : kind === 'message' ? this.onmessage : kind === 'error' ? this.onerror : this.onclose;
    (handler as ((ev: Event) => unknown) | null)?.call(this, ev);
    this.dispatchEvent(ev);
  }
}

/**
 * Routes construction to the demo class for API URLs and to the real one for everything else (Vite's
 * HMR socket, most importantly). A Proxy rather than a plain function so the statics the portal
 * reads - `WebSocket.OPEN` - keep working without being restated.
 */
function routeConstructor<T extends object>(real: T, isDemo: (url: string) => boolean, Demo: new (url: never) => unknown): T {
  return new Proxy(real, {
    construct(target, args: unknown[], newTarget) {
      const url = String(args[0] ?? '');
      if (isDemo(url)) return new Demo(url as never) as object;
      return Reflect.construct(target as never, args, newTarget) as object;
    },
  });
}

// --- links that leave the page ------------------------------------------------

/**
 * Two portal surfaces open a new tab rather than making a request, and a patched fetch cannot help
 * either of them:
 *
 *   - the "Open UI" links on the Monitoring and Storage pages, which navigate to the API's tunnel
 *     endpoint (/api/clusters/{id}/proxy/{app}/). Here that endpoint IS servable - the fake tunnel
 *     returns a self-contained page - it just has to come from the module rather than the network,
 *     so the click is intercepted and the response shown from a blob URL.
 *   - the Secrets page's "View in Vault" handoff, which opens whatever address the deployment's
 *     Vault lives at. There is no Vault behind this demo and no address that would be true, so the
 *     new tab gets an explanation instead of a connection error.
 *
 * Both open the tab synchronously, before any await, or the popup blocker takes it.
 */
function installLinks() {
  document.addEventListener(
    'click',
    (ev) => {
      if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey) return;
      const anchor = (ev.target as Element | null)?.closest?.('a[href]') as HTMLAnchorElement | null;
      if (!anchor) return;
      const path = apiPath(anchor.getAttribute('href') ?? '');
      if (path === null) return;
      ev.preventDefault();
      // Deliberately without `noopener`: that makes window.open return null by spec, and the handle
      // is the whole point - the tab is navigated to a blob of our own making.
      const tab = window.open('', '_blank');
      if (!tab) return;
      void fetch(anchor.href)
        .then((res) => res.text())
        .then((html) => showInTab(tab, html))
        .catch((err) => showInTab(tab, notePage('Could not open this view', String(err))));
    },
    true,
  );

  const realOpen = window.open.bind(window);
  window.open = ((url?: string | URL, target?: string, features?: string) => {
    const href = url === undefined ? '' : String(url);
    if (!href || apiPath(href) !== null || sameHost(href)) {
      return realOpen(url as string, target, features);
    }
    // Same reason as above: the features string is dropped so a handle comes back.
    const tab = realOpen('', target ?? '_blank');
    if (tab)
      showInTab(
        tab,
        notePage(
          'Not part of the browser demo',
          `This link goes to <code>${escapeHTML(href)}</code> - a service the platform integrates with,
           which has no counterpart here. Everything else on this page is the real control plane
           running in your browser; only the things it would talk to over a network are missing.`,
        ),
      );
    return tab;
  }) as typeof window.open;
}

/** True when href points at this page's own host; an unparseable href is treated as ours. */
function sameHost(href: string): boolean {
  try {
    return new URL(href, location.href).host === location.host;
  } catch {
    return true;
  }
}

function showInTab(tab: Window, html: string) {
  const url = URL.createObjectURL(new Blob([html], { type: 'text/html' }));
  tab.location.replace(url);
  // Revoking immediately would race the navigation; a minute is far longer than it needs.
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

function escapeHTML(s: string) {
  return s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c] ?? c);
}

function notePage(title: string, body: string) {
  return `<!doctype html><html><head><meta charset="utf-8"><title>${escapeHTML(title)}</title>
<style>body{font:400 15px/1.6 system-ui,sans-serif;background:#0b0f19;color:#e6e9ef;margin:0;display:flex;
min-height:100vh;align-items:center;justify-content:center}.card{max-width:34rem;padding:2.5rem;border:1px solid
#263041;border-radius:14px;background:#111726}h1{margin:0 0 1rem;font-size:1.4rem}p{color:#8b93a7;margin:0}
code{font-family:ui-monospace,monospace;color:#c9d1e1;word-break:break-all}</style></head>
<body><div class="card"><h1>${escapeHTML(title)}</h1><p>${body}</p></div></body></html>`;
}

/** install patches the three APIs. Call once, before the portal mounts. */
export function installShim() {
  installFetch();
  installLinks();
  window.EventSource = routeConstructor(
    window.EventSource,
    (url) => apiPath(url) !== null,
    DemoEventSource as never,
  ) as typeof EventSource;
  window.WebSocket = routeConstructor(
    window.WebSocket,
    (url) => apiPath(url) !== null,
    DemoWebSocket as never,
  ) as typeof WebSocket;
}
