# Fake mode

Fake mode runs the **entire** platform - the portal, the API, the reconciliation loop, the state
machine, the durable queue, multi-tenancy, and every portal feature - against **simulated backends**.
Clusters go from `Pending` to `Ready` with no VM created anywhere.

It's the fastest way to see KubeHarbor, the way to develop it, and how the platform is demonstrated
without a hypervisor. Because the seams are real interfaces with fake implementations behind them (see
[Architecture](../concepts/architecture.md#the-seams-fake-vs-real)), what you're exercising is the
*actual* control plane - only the parts that would touch a hypervisor, a cluster, or a directory are
simulated.

## Run it

As containers (portal + API + Postgres + a dev Vault, no worker):

```bash
make up-fake      # portal at http://localhost:8080, API on :8081
make logs-fake
make down-fake
```

Sign in as **`admin` / `admin`**. Grant your account some quota on the Administration page (or set
`KAAS_SHARED_QUOTA=true`), then create clusters and watch them converge.

Without containers, for local development:

```bash
make run-api      # the API + in-process reconciler with fake providers on :8080
make run-worker   # seed one demo cluster and log it going Pending → Ready
make test         # unit tests: the state machine converges, steps are idempotent
```

For hot-reload portal work, run `make run-api` and, alongside it, `make web-dev` (Vite dev server on
`:5173`, proxying `/api` to the API). See the [portal README](../../web/README.md).

## What fake mode exercises

Everything except the four things that need real infrastructure:

| Real, even in fake mode | Simulated in fake mode |
|---|---|
| the reconcile loop and state machine | VM provisioning (pretend IPs) |
| catalog, quota, and admission | cluster formation (a plausible kubeconfig) |
| events / SSE, the Activity timeline | add-on installs (instant success) |
| multi-tenancy, groups, per-user quota | metrics, health, workloads, monitoring, security |
| Postgres persistence + the durable queue | the terminal, node SSH, and DNS/Vault |

## Demoing the full flows without hardware

Several integrations have a fake that lets you demo the *whole* flow with no external system:

- **Multiple infrastructures** - set `KAAS_INFRA_PROVIDERS=kvm,vsphere,proxmox` and every provider maps
  to the same fake, so the wizard's Infrastructure step, provider badges, and per-provider quota all
  work.
- **Directory auth** - `KAAS_AUTH=ldap KAAS_LDAP=fake` runs the entire Active Directory flow (group
  seeding, just-in-time provisioning, membership sync) against an in-memory directory synthesized from
  your real mapping rules. See [Directory authentication](integrations/directory-auth.md).
- **Vault** - the fake Vault records state in memory, so the Secrets page and the "View in Vault"
  handoff are demoable. (The Compose stack also ships a real dev Vault container; set `KAAS_VAULT=real`
  to use it.)

## When you're ready for real clusters

Move on to [Podman Compose](compose.md) (single machine) or [Helm](helm.md) (an existing cluster), and
build [golden images](golden-images.md) for your provider.
