import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

// The SPA talks to the same-origin control-plane API under `/api`. In production nginx serves
// this build and reverse-proxies `/api/*` to the Go API (see deploy/nginx.conf). In dev, Vite
// proxies `/api` to a locally-running `make run-api` on :8080, stripping the prefix so the Go
// routes (which live at the root: /clusters, /catalog, …) match. SSE works through both.
export default defineConfig({
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
    sourcemap: false,
  },
});
