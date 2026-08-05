# The browser demo

KubeHarbor publishes itself as a **static website**: the portal, plus the entire control plane
compiled to WebAssembly and running inside the visitor's tab. No server, no database, no hypervisor -
and nothing to sign up for.

It is not a set of screenshots or a mocked API. It is `cmd/api` in a different wrapper: the same
`internal/app`, the same REST routes, the same reconciliation loop and cluster state machine, running
against the in-memory store and the fake seams that [fake mode](fake-mode.md) already uses. Clusters
you create in it are really admitted, really charged against quota, and really walk the phases from
`Pending` to `Ready`.

## Try it

The published site is deployed from `main` by
[`.github/workflows/pages.yml`](../../.github/workflows/pages.yml). Sign in with any of:

| Account | Password | What it shows |
|---|---|---|
| `admin` | `kubeharbor` | Platform admin: every tenant's clusters, quota grants, groups |
| `alice` | `kubeharbor` | A tenant with capacity, owning the seeded fleet |
| `bob` | `kubeharbor` | A read-only member of alice's group: same clusters, no changes |

The demo seeds itself before the portal appears: two KVM clusters (one HA, one with two node pools),
one on vSphere, one on Proxmox, and a fifth still being provisioned so the first screen has a live
event feed on it. That takes a couple of seconds, which is what the loading screen is for.

## Run it locally

```bash
make demo-dev      # Vite dev server with the wasm control plane; no API needed
make demo-build    # the complete static site into web/portal/dist
```

`make demo-build VITE_BASE=/KaaS-Demo/` builds for a GitHub *project* page, which is served from a
subpath rather than a domain root.

## What works, and what cannot

Working, for real: the create wizard, node pools, node disks, add-on selection and values, bundle
upgrades, quota and capacity, multi-tenancy (users, groups, the read/write role), the SSE event
timeline, health, metrics, Workloads, Storage, Networking, Monitoring, Security, Audit, Secrets,
custom catalogs, the cluster terminal, node SSH, and live pod logs.

Not present, because these are exactly the things the fakes stand in for: OpenTofu, Ansible, Helm, a
hypervisor, Vault, a directory, DNS, NetBox. The "Open UI" links serve the same synthesized landing
page fake mode does, and the "View in Vault" handoff explains itself rather than opening a dead tab.

**State is per-tab and resets on reload.** The store is in memory, so a refresh re-seeds the fleet
from scratch - which takes about as long as the first load did.

## How it works

Three pieces:

- **[`cmd/demo-wasm`](../../cmd/demo-wasm)** - the browser entry point. It builds the app exactly as
  `cmd/api` does (every seam forced to its fake), starts the reconciler, seeds the demo fleet, and
  exposes two functions to the page: one that drives an HTTP request through `api.Routes()`, and one
  that opens a terminal session.
- **[`web/portal/src/demo/`](../../web/portal/src/demo)** - the shim. It patches `fetch`,
  `EventSource` and `WebSocket` so that requests to `/api/*` go into the module instead of onto the
  network. Nothing else in the portal knows the demo exists; `lib/api.ts` and every page are
  unchanged.
- **the build tags** - four packages cannot compile for `js/wasm` (the three exec-agent proxies,
  because a browser WebSocket handshake carries no headers, and `internal/shell/pty`, because a
  browser has no pseudo-terminals). Each has a small `_js` counterpart. CI builds
  `GOOS=js GOARCH=wasm ./cmd/demo-wasm` on every PR so those cannot rot unnoticed.

Two details are load-bearing and easy to get wrong if you touch this:

- **The session cookie lives in the shim, not the browser.** A browser ignores `Set-Cookie` on a
  `Response` that JavaScript constructed, so the shim keeps the value and re-attaches it as an
  ordinary `Cookie` header. The Go side is unchanged and unaware.
- **Bridge callbacks are deferred by a microtask.** Go's wasm scheduler runs a newly spawned
  goroutine before returning control to the JS caller, so an undeferred bridge call would deliver its
  first callback *during* the `new EventSource(...)` / `new WebSocket(...)` line - before the caller
  has assigned `onopen`/`onmessage`. Neither real API can fire that early.

The module is ~46 MB, shipped pre-compressed (~8 MB) and inflated by the page, because Pages does not
negotiate an encoding for `application/wasm`.

## Why a page-hosted module and not a service worker

A service worker would let the tunnel links and downloads be ordinary navigations. But service
workers are terminated when idle, which would stop the reconcile loop and discard the store - fatal
for a demo whose entire point is a control plane that keeps converging while you watch it.

*Production would* - if this were a product demo rather than a portfolio one - persist the store to
IndexedDB so a reload keeps your fleet, and trim the module by build-tagging out the Postgres store,
govmomi, LDAP and WinRM, which together are about a third of its size and are unreachable in this
build anyway.
