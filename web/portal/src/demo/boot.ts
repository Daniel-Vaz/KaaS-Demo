// Boot for the static demo build: load the WebAssembly control plane, wait for it to seed its fleet,
// and only then let the portal mount.
//
// The ordering is the point. cmd/demo-wasm creates the demo clusters and waits for the reconciler to
// converge them before it signals ready, so the first screen a visitor sees is a running platform
// rather than an empty table that fills in underneath them. That costs a couple of seconds, which is
// what the splash below is for.

import { installShim } from './shim';

/** Where the workflow (and `make demo-wasm`) puts the module, relative to the site root. */
const WASM_GZ = 'demo/kaas-demo.wasm.gz';
const WASM = 'demo/kaas-demo.wasm';
const WASM_EXEC = 'demo/wasm_exec.js';

export async function bootDemo(): Promise<void> {
  const splash = showSplash();
  try {
    installShim();
    await loadScript(asset(WASM_EXEC));

    splash.say('Loading the control plane…');
    const go = new Go();
    const { instance } = await WebAssembly.instantiate(await loadModule(), go.importObject);

    const ready = new Promise<void>((resolve, reject) => {
      globalThis.__kaasBoot = (state, detail) =>
        state === 'ready' ? resolve() : reject(new Error(detail ?? 'the control plane failed to start'));
    });

    splash.say('Starting the reconciler…');
    // go.run resolves only when main returns, which it never does - the module stays alive for the
    // page. The readiness signal is what we wait on.
    void go.run(instance);

    splash.say('Provisioning the demo clusters…');
    await ready;
    splash.remove();
  } catch (err) {
    splash.fail(err instanceof Error ? err.message : String(err));
    throw err;
  }
}

function asset(path: string): string {
  return import.meta.env.BASE_URL + path;
}

/**
 * Fetches the module, preferring the pre-compressed copy.
 *
 * GitHub Pages decides its own Content-Encoding and does not negotiate one for application/wasm, so
 * shipping a .gz and inflating it here is the difference between an 8 MB download and a 46 MB one.
 * The plain file is the fallback for a local build that skipped the compression step.
 */
async function loadModule(): Promise<ArrayBuffer> {
  const gz = await fetch(asset(WASM_GZ));
  if (gz.ok && gz.body) {
    return new Response(gz.body.pipeThrough(new DecompressionStream('gzip'))).arrayBuffer();
  }
  const plain = await fetch(asset(WASM));
  if (!plain.ok) throw new Error(`could not load the demo control plane (${plain.status})`);
  return plain.arrayBuffer();
}

function loadScript(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const el = document.createElement('script');
    el.src = src;
    el.onload = () => resolve();
    el.onerror = () => reject(new Error(`could not load ${src}`));
    document.head.appendChild(el);
  });
}

// --- splash ------------------------------------------------------------------

function showSplash() {
  const root = document.createElement('div');
  root.id = 'kaas-demo-splash';
  root.innerHTML = `
    <style>
      #kaas-demo-splash {
        position: fixed; inset: 0; z-index: 9999;
        display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 18px;
        background: #0b0d12; color: #c9d1e1;
        font: 400 14px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
      }
      #kaas-demo-splash .mark { font-size: 22px; font-weight: 650; color: #f2f5fa; letter-spacing: -0.01em; }
      #kaas-demo-splash .mark span { color: #0f61f0; }
      #kaas-demo-splash .tag { color: #7d8798; font-size: 13px; margin-top: -12px; }
      #kaas-demo-splash .bar { width: 240px; height: 3px; border-radius: 3px; background: #1b2130; overflow: hidden; }
      #kaas-demo-splash .bar i { display: block; height: 100%; width: 40%; border-radius: 3px; background: #0f61f0;
        animation: kaas-slide 1.1s ease-in-out infinite; }
      @keyframes kaas-slide { 0% { transform: translateX(-100%); } 100% { transform: translateX(250%); } }
      #kaas-demo-splash .msg { min-height: 20px; color: #7d8798; }
      #kaas-demo-splash.failed .bar { display: none; }
      #kaas-demo-splash.failed .msg { color: #ff6b6b; max-width: 32rem; text-align: center; }
    </style>
    <div class="mark">Kube<span>Harbor</span></div>
    <div class="tag">Kubernetes Without the Rough Seas</div>
    <div class="bar"><i></i></div>
    <div class="msg">Starting the browser demo…</div>
  `;
  document.body.appendChild(root);
  const msg = root.querySelector('.msg') as HTMLElement;

  return {
    say: (text: string) => {
      msg.textContent = text;
    },
    fail: (text: string) => {
      root.classList.add('failed');
      msg.textContent = text;
    },
    remove: () => root.remove(),
  };
}
