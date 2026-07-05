# Web portal

The **KubeHarbor** portal - a Kubernetes-as-a-Service console built with **React + TypeScript + Vite +
[Mantine](https://mantine.dev)**, living in [`portal/`](portal/). It talks to the control-plane API
over same-origin JSON + SSE + WebSocket under `/api`.

In production it's served by its own nginx container ([`deploy/Containerfile.web`](../deploy/Containerfile.web)
+ [`deploy/nginx.conf`](../deploy/nginx.conf)), which serves the built assets and reverse-proxies
`/api/*` to the Go API. It's the front door at **http://localhost:8080**; the API is on **:8081** for
direct curl. `make up` / `make up-fake` build and run it - no separate step.

The full, image-heavy walkthrough of every page is the [**portal user guide**](../docs/portal/README.md).

## What it does

The console covers the whole platform:

- **Overview** - fleet dashboard: cluster counts, per-infrastructure quota, phase distribution.
- **Clusters** - the list, the create wizard, and the per-cluster detail page (Overview, Nodes +
  disks + SSH, Terminal, Add-ons, Activity, Audit, Upgrades).
- **Workloads / Storage / Networking** - browse and scale live workloads, inspect PVCs and
  StorageClasses, and see Services, Gateways, Routes, and exposed apps.
- **Monitoring / Security** - native Prometheus dashboards and Trivy security reports.
- **Secrets** - the Vault-backed per-cluster secret store.
- **Catalog** - the add-on library and user-defined custom catalogs.
- **Administration** - accounts, groups, and quota (admin only); **Profile** for your own account.

State and polling are handled by **TanStack Query** (lists/details poll every couple of seconds so the
reconciler's progress shows up live); the activity stream uses the browser's native `EventSource`, and
the terminal/SSH tabs use WebSockets.

## Developing

`make up-fake` is the quickest way to see the whole thing. For hot-reload UI work:

```bash
make run-api      # the Go API + reconciler with fake providers on :8080
make web-install  # once: install portal deps (needs Node 18+)
make web-dev      # Vite dev server on http://localhost:5173, proxying /api -> :8080
```

`make web-build` runs the type-check and production build. The API stays a pure JSON + SSE surface - to
drive it with curl instead, see the [portal user guide](../docs/portal/README.md) and the
[configuration reference](../docs/deploy/configuration.md).
