import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

// The SPA talks to the same-origin control-plane API under `/api`. In production nginx serves
// this build and reverse-proxies `/api/*` to the Go API (see deploy/nginx.conf). In dev, Vite
// proxies `/api` to a locally-running `make run-api` on :8080, stripping the prefix so the Go
// routes (which live at the root: /clusters, /catalog, …) match. SSE works through both.
//
// The static demo build (VITE_DEMO=1) has no API at all: the control plane is a WebAssembly module
// in the page and the shim in src/demo/ intercepts those same `/api` URLs before they reach the
// network. It only needs two build-level differences, both because GitHub Pages serves a project
// site from a subpath rather than a domain root:
//   - `base`, so asset URLs are absolute-correct under /<repo>/ (BASE_URL also feeds the router's
//     basename and the wasm asset paths in src/demo/boot.ts);
//   - a bigger inline-asset threshold is NOT wanted - the module must stay a separate file so the
//     browser can stream and cache it, which is why it lives in public/demo/ rather than being
//     imported.
export default defineConfig(({ mode }) => ({
  // VITE_BASE is the site's public path ("/KaaS-Demo/" for a GitHub project page); unset means the
  // domain root, which is every other deployment.
  base: process.env.VITE_BASE || '/',
  // Defined explicitly rather than left to .env-file loading, so the flag is set by exactly one
  // thing: the environment the build ran in (see the Makefile's demo-portal target).
  define: { 'import.meta.env.VITE_DEMO': JSON.stringify(process.env.VITE_DEMO ?? '') },
  plugins: [react()],
  resolve: {
    alias: { '@': path.resolve(__dirname, 'src') },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        ws: true, // bridge the cluster-shell WebSocket (and SSE) to a local `make run-api`
        rewrite: (p) => p.replace(/^\/api/, ''),
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: mode === 'development',
  },
}));
