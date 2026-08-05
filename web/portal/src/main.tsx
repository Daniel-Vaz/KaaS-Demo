import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import { MantineProvider, ColorSchemeScript } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { ModalsProvider } from '@mantine/modals';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import '@mantine/core/styles.css';
import '@mantine/notifications/styles.css';
import '@mantine/charts/styles.css';
import './styles.css';

import { theme } from './theme';
import { App } from './App';
import { AuthProvider } from './lib/auth';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false },
  },
});

function render() {
  ReactDOM.createRoot(document.getElementById('root')!).render(
    <React.StrictMode>
      <ColorSchemeScript defaultColorScheme="dark" />
      <MantineProvider theme={theme} defaultColorScheme="dark">
        <Notifications position="top-right" />
        <QueryClientProvider client={queryClient}>
          <AuthProvider>
            <ModalsProvider>
              <BrowserRouter basename={import.meta.env.BASE_URL}>
                <App />
              </BrowserRouter>
            </ModalsProvider>
          </AuthProvider>
        </QueryClientProvider>
      </MantineProvider>
    </React.StrictMode>,
  );
}

// In the static demo build (VITE_DEMO=1, see web/portal/src/demo/) there is no API server: the whole
// control plane is a WebAssembly module in this page, reached through a shim that patches fetch,
// EventSource and WebSocket. It has to be running - and to have seeded its clusters - before
// anything renders, so the portal mounts after it and not before. The import is dynamic so an
// ordinary build never pulls the demo code in at all.
if (import.meta.env.VITE_DEMO) {
  void import('./demo/boot').then(({ bootDemo }) => bootDemo().then(render));
} else {
  render();
}
