# Architecture

KubeHarbor is **a control plane driven by a level-triggered reconciliation loop** - the same shape
Kubernetes and Cluster API use, applied to whole clusters instead of pods. This page explains that
shape and the pieces that make it up.

## The mental model

1. A create, update, or delete request from the portal or API becomes a **desired-state record** -
   a cluster row (plus its node pools, add-ons, and disks) in Postgres. The API only ever *writes
   desired state*; it never provisions anything itself.
2. A **reconciler** continuously reads the clusters that still need work and advances each one **one
   idempotent step at a time** through a lifecycle state machine, calling out to backends that create
   VMs, form the cluster, and install add-ons.
3. Progress is **level-triggered**: the loop re-derives what to do from desired vs. observed state on
   every tick. That makes it self-healing - a crash mid-provision is simply retried, a VM that
   disappears is recreated, and infrastructure whose cluster is gone is swept away.

The signal for "there is work to do" is the pair `generation` / `observed_generation`. Every user
edit bumps `generation`; the reconciler keeps working a cluster until `observed_generation` catches up
and the phase is `Ready`.

| KubeHarbor | Kubernetes / Cluster API |
|---|---|
| cluster rows in Postgres | custom resources in etcd |
| the reconciler (`internal/reconcile`) | controllers / controller-manager |
| the cluster phase machine | Cluster / Machine phases |
| `generation` / `observed_generation` | the same fields, the same semantics |
| OpenTofu + libvirt/vSphere/Proxmox | an infrastructure provider |
| Ansible + kubeadm | a bootstrap provider |

## The components

```
┌──────────────────────────────────────────────────────────────────┐
│  Portal (React + Mantine SPA)                       [web]          │
│  create wizard · fleet & cluster views · live event log · console  │
└───────────────────────────────┬──────────────────────────────────┘
                                 │  REST + SSE + WebSocket, same origin
┌───────────────────────────────▼──────────────────────────────────┐
│  API (Go)                                           [api]          │
│  authenticate · authorize · enforce quota · resolve catalog ·      │
│  write desired state · stream events · never touches a hypervisor  │
└───────────────────────────────┬──────────────────────────────────┘
                                 │
┌────────────────────────┐      │      ┌──────────────────────────────┐
│  Postgres        [pg]   │◄─────┴──────┤  Reconciler / worker  [worker]│
│  desired + observed     │             │  the ONLY component that       │
│  state · durable job    │             │  touches the hypervisor        │
│  queue · event relay    │             └──────┬───────────┬───────────┘
└─────────────────────────┘                    │ OpenTofu  │ Ansible / Helm
                                        ┌───────▼───────────▼──────────┐
                                        │  libvirt / vSphere / Proxmox  │
                                        │  the cluster VMs              │
                                        └───────────────────────────────┘
```

Two more small, isolated components serve the portal's interactive features (the in-browser terminal,
node SSH, workloads, logs) without putting them on the privileged worker - the **shell sandbox** and
the **node-ssh sandbox**. See [the terminal & SSH tools](../portal/managing-clusters.md) and
[Compose](../deploy/compose.md) for how they're wired.

### Two processes, one wiring

Both entry points build the same application object, which selects backends from environment variables
and wires the reconciler:

- **`cmd/api`** serves the REST + SSE + WebSocket surface. In real deployments it does **not**
  reconcile - it only writes desired state.
- **`cmd/worker`** runs the reconciler headless. It is the **only** process that reaches the
  hypervisor and the cluster VMs, so it holds the credentials, the SSH keys, and (on local KVM) the
  libvirt socket. Everything with a blast radius lives here.

They share state through Postgres, so they're genuinely decoupled: the API bumps desired state, the
worker reconciles it.

## The seams: fake vs. real

The control-plane logic depends only on small interfaces ("seams"). Each has a **fake** implementation
- so the whole platform runs and is tested with no hypervisor, no database, and no cluster - and a
**real** one. An environment variable selects which to use, per seam. This is what makes [fake
mode](../deploy/fake-mode.md) a faithful demo rather than a mock-up: it runs the *real* reconcile
loop, state machine, and portal against simulated backends.

| Seam | Selector | Fake | Real |
|---|---|---|---|
| Persistence + job queue | `DATABASE_URL` | in-memory | Postgres + River |
| Directory auth | `KAAS_AUTH` / `KAAS_LDAP` | in-memory directory | Active Directory / LDAP |
| VM provisioning | `KAAS_PROVISIONER` | pretend IPs | OpenTofu on libvirt / vSphere / Proxmox |
| Cluster formation | `KAAS_CONFIG` | plausible kubeconfig | Ansible + kubeadm |
| Add-ons | `KAAS_ADDONS` | instant success | Helm |
| Usage metrics | `KAAS_METRICS` | synthetic load | `kubectl` → metrics API |
| Health checks | `KAAS_HEALTH` | all-healthy | `kubectl` → API-server checks |
| Cluster shell | `KAAS_SHELL` | simulated kubectl | worker-proxied bash + kubectl PTY |
| Node SSH | `KAAS_NODE_SSH` | simulated Linux shell | sandbox-proxied `ssh` |
| Workloads / storage / networking | `KAAS_KUBE` | synthesized objects | worker-proxied `kubectl` |
| Monitoring | `KAAS_MONITORING` | synthetic telemetry | worker-proxied PromQL |
| Security | `KAAS_SECURITY` | synthetic reports | worker-proxied Trivy CRD reads |
| Audit | `KAAS_AUDIT` | synthetic events | worker-proxied apiserver-log reads |
| In-cluster UI tunnel | `KAAS_TUNNEL` | synthetic landing page | worker-proxied HTTP reverse proxy |
| Cluster DNS | `KAAS_DNS` | logs what it would publish | `nsupdate` (RFC 2136) or WinRM |
| Secret store (Vault) | `KAAS_VAULT` | in-memory | HashiCorp Vault |

The full selection reference is in the [configuration guide](../deploy/configuration.md).

## The cluster lifecycle

The reconciler advances a cluster **at most one phase per invocation**, so progress is visible step by
step in the [Activity log](../portal/managing-clusters.md). The bring-up path:

```
Pending → ProvisioningInfra → InfraReady → ControlPlaneReady → WorkersReady
        → InstallingAddons → Ready
```

- **ProvisioningInfra** - create the VMs (idempotent `EnsureNodes`).
- **InfraReady** - `kubeadm init` on the first control plane; for HA, stand up the keepalived/haproxy
  VIP and join the other control planes. The admin kubeconfig and a worker join token are stored
  encrypted.
- **ControlPlaneReady** - `kubeadm join` every worker.
- **WorkersReady** - install the CNI, mount the default storage disks, mint the read-only viewer
  kubeconfig.
- **InstallingAddons** - install add-ons with Helm, then wire monitoring, the default gateway/TLS,
  storage, and DNS.

`Ready` is not final. When `observed_generation` falls behind `generation`, or a periodic concern
comes due, the cluster leaves `Ready` for the appropriate phase and returns:

| Phase | Trigger |
|---|---|
| `Updating` | node pools, add-ons, or disks edited |
| `Upgrading` | a version-bundle promotion - in-place kubeadm, rolling node replacement, or Helm |
| `Repairing` | a node is faulty and the repair policy agreed to act |
| `RenewingCerts` | control-plane certificates are within the renewal window |
| `SnapshottingEtcd` | a periodic control-plane backup is due |
| `DefragmentingEtcd` | etcd is fragmented past the thresholds |
| `Deleting` → `Deleted` | the cluster was deleted |

The last five are what make the platform *keep* clusters healthy - see [Keeping clusters
healthy](keeping-clusters-healthy.md).

## Reconciliation and the job queue

There are two ways the loop runs, both calling the same one-phase-per-invocation step:

- **In-memory tick loop** (no `DATABASE_URL`) - a ticker advances each cluster that needs work. Simple
  and non-durable; used in fake local dev.
- **River durable queue** (with Postgres) - one unique job per cluster that needs work; a pool of
  workers each execute one phase, with exponential-backoff retries on failure. Job uniqueness is
  scoped per cluster, so exactly one job per cluster is in flight platform-wide, however many workers
  run.

A slower, leader-elected sweep handles orphan garbage collection (destroying infrastructure whose
cluster is gone), plus the metrics and health samplers.

## Multi-tenancy

Every cluster is owned by a user. A normal user sees and manages only their own clusters (and their
group-mates'); an **admin** sees everything and manages accounts, groups, and quota.

- **Accounts** come from local password auth or an [Active Directory / LDAP
  directory](../deploy/integrations/directory-auth.md). New accounts start at zero quota.
- **Groups** are teams: members share access to each other's clusters under a coarse **read/write
  role** that is scoped per group (a user can hold different roles in different groups).
- **Capacity quota** is a first-class admission check, **per user, per infrastructure** - the KVM
  host's cores and the vCenter's cores are different physical capacity and are never fungible. See
  [the account & admin guide](../portal/account-and-admin.md).
- Every interactive kubectl surface (the download, the shell, workloads/scale) runs as a **per-user
  credential**: a cluster-CA-signed client cert carrying the user's own login and their resolved role,
  so cluster API access matches the identity the portal resolved and the audit trail records the real
  username.

The reconcile loop itself is **untenanted** - it reconciles every cluster platform-wide, because
desired state doesn't care who owns it. Tenancy is purely an admission/authorization concern at the
edge.

## Horizontal scaling

Every stateless tier - `web`, `api`, `worker`, and the exec-agent sandboxes - runs with as many
replicas as you like ([`make up-scale`](../deploy/compose.md) or the [Helm chart](../deploy/helm.md)).
Postgres is the single, deliberate exception. Correctness rests on four rules:

- **Reconcile work is claimed, not assigned** - River's per-cluster job uniqueness means one job per
  cluster runs platform-wide.
- **Singleton sweeps are leader-elected** - the GC/metrics/health loops run only in the replica
  holding a Postgres advisory lease.
- **Admission is serialized** - quota and network IPAM decisions take a Postgres advisory lock, with a
  schema-level unique constraint as backstop.
- **Nothing is pinned to a replica** - signed-cookie sessions, Postgres `LISTEN`/`NOTIFY` for the
  event stream, and stateless exec agents mean any request can land on any replica.

## Secrets and encryption

Kubeconfigs, join tokens, SSH material, and control-plane backups are encrypted at rest with
AES-256-GCM before they reach Postgres. For a richer, tenant-facing secret store, the platform
integrates [HashiCorp Vault](../deploy/integrations/vault.md): each cluster gets its own KV subtree and
policies that mirror the portal's read/write model, consumed inside the cluster by the External Secrets
Operator and surfaced on the portal's [Secrets page](../portal/secrets.md).

## Where to go next

- The moving parts of provisioning, bring-up, and upgrades are covered per layer in the [provider
  guides](../deploy/providers/libvirt.md).
- The self-healing behaviours are in [Keeping clusters healthy](keeping-clusters-healthy.md).
- The full runtime configuration surface is the [configuration reference](../deploy/configuration.md).
- The deep rationale behind individual decisions lives in [`CLAUDE.md`](../../CLAUDE.md) and the code
  comments.
