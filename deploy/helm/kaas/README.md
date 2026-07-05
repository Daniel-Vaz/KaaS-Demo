# `kaas` - the KubeHarbor control plane on Kubernetes

Deploys the KubeHarbor portal, the JSON+SSE API, the reconciler worker, the unprivileged cluster-shell
sandbox, Vault, and (optionally) Postgres. Every tier runs with **multiple replicas**; the chart exists
because the platform is horizontally scalable, and this is where that pays off.

> This README is the chart's own reference. For the deployment walkthrough - modes, the two SSH keys,
> and the sharp edges - see the [**Helm deployment guide**](../../../docs/deploy/helm.md).

```
                    ┌──────────┐
   browser ────────▶│ Ingress  │
                    └────┬─────┘
                         │
                  ┌──────▼──────┐        ┌──────────────┐
                  │  web ×N     │───────▶│   api ×N     │  JSON + SSE; no KVM, ever
                  │  (nginx SPA)│  /api  │              │
                  └─────────────┘        └───┬──────┬───┘
                                             │      │ Terminal / Workloads /
                                             │      │ Monitoring / Security
                            ┌────────────────▼─┐  ┌─▼──────────────┐
                            │   postgres (1)   │  │   shell ×N     │  bash + kubectl only,
                            │  state · queue · │  │  (sandbox)     │  no platform secrets
                            │  advisory locks  │  └─────┬──────────┘
                            └────────▲─────────┘        │
                                     │                  │ kubectl → SOCKS
                             ┌───────┴──────┐   ┌───────▼────────┐
                             │  worker ×N   │   │  socks ×N      │  the ONLY pods holding
                             │  reconciler  │──▶│  (SSH tunnel)  │  the hypervisor key
                             └──────────────┘   └───────┬────────┘
                                                        │
                                                  KVM host (VMs)
```

## Why every tier can be replicated

Coordination lives in Postgres, not in any process - see
[Architecture](../../../docs/concepts/architecture.md#horizontal-scaling):

- **worker** - per-cluster work is claimed from River's durable queue, unique by cluster id, so at
  most one job per cluster is in flight platform-wide. The loops that are *not* per-cluster (orphan
  GC, metrics, health) run only in the replica holding a Postgres advisory lease; a leader that dies
  drops the lock and another takes over in seconds.
- **api** - signed-cookie sessions (no server-side store) and a Postgres `LISTEN`/`NOTIFY` event
  stream, so an SSE client on *any* replica sees events from *any* worker. Quota/IPAM admission is
  serialized by an advisory lock, so concurrent creates can't double-allocate a subnet.
- **web** - stateless nginx that re-resolves the API Service per request.
- **shell** - no state between requests (each carries its own kubeconfig), so the API just points at
  the Service and Kubernetes spreads the load.
- **postgres** - *not* replicated. It is the source of truth, and real HA Postgres is out of scope
  for the demo. Point `postgres.external.dsn` at a managed instance for anything real.

## Try it with no hypervisor

`providers=fake` runs the **same** control plane - reconcile loop, state machine, durable queue,
leader election, advisory-locked admission - against simulated backends. Clusters go Pending → Ready
with no VM anywhere. It is the Kubernetes counterpart of `make up-fake`, and the way to see the chart
work on any cluster:

```bash
helm install kaas deploy/helm/kaas --set providers=fake
kubectl port-forward svc/kaas-web 8080:80    # → http://localhost:8080
```

## A real install

Real mode provisions actual VMs, so it needs a hypervisor and two SSH keypairs - one for the
**hypervisor** (`kvm.ssh.privateKey`), one for the **cluster VMs** the platform creates
(`config.clusterSSH`). Do not confuse them; they are different keys with different blast radii.

```bash
helm install kaas deploy/helm/kaas \
  --set kvm.host=192.168.1.50 \
  --set-file kvm.ssh.privateKey=/path/to/hypervisor_key \
  --set-file config.clusterSSH.privateKey=/path/to/vm_key \
  --set-file config.clusterSSH.publicKey=/path/to/vm_key.pub \
  --set ingress.enabled=true --set ingress.host=kaas.example.com
```

`kvm.mode` decides the topology, and it is the one choice that shapes everything:

| | `remote` (default) | `local` |
|---|---|---|
| Where the VMs are | another machine, reached over SSH | the node the pod lands on |
| Worker/shell pods | ordinary pods - no host network, no host paths, freely scheduled and scaled | `hostNetwork`, the node's libvirt socket as a `hostPath`, pinned by `kvm.local.nodeSelector` |
| Shell replicas | any number | one per libvirt node (they all want `:8082` on the host) |
| Use it when | always, in Kubernetes | single-node / dev cluster only |

## vSphere and Proxmox

`kvm` is always available; add `vsphere` and/or `proxmox` to `infra.providers` to offer them too -
they need no `hostNetwork`/tunnel dance because the worker reaches vCenter/Proxmox and the cluster
VMs directly, and their OpenTofu modules are baked into the worker image (no volume mount, unlike
KVM's libvirt socket):

```bash
helm install kaas deploy/helm/kaas \
  --set 'infra.providers={kvm,proxmox}' \
  --set infra.proxmox.endpoint=https://proxmox.example.internal:8006/ \
  --set infra.proxmox.node=proxmox01 \
  --set infra.proxmox.datastore=Pool3ParNew \
  --set infra.proxmox.apiToken='kaas@pve!tofu=...' \
  --set infra.proxmox.network.bridge=vmbr0 \
  --set infra.proxmox.network.mode=static \
  --set infra.proxmox.network.cidr=172.23.234.0/24 \
  --set infra.proxmox.network.range=172.23.234.50-172.23.234.70
```

Same shape for `vsphere` - see the commented `infra.vsphere`/`infra.proxmox` blocks in
[`values.yaml`](values.yaml). Both are "shared-network" providers (see the
[vSphere](../../../docs/deploy/providers/vsphere.md) and
[Proxmox](../../../docs/deploy/providers/proxmox.md) guides): one operator-owned network, dhcp or platform-allocated static
addressing, and their own `budget` - capacity is not fungible between backends, so the KVM host's
spare cores can't fund a vSphere or Proxmox VM. `infra.netbox` optionally records the addresses
either one occupies in NetBox IPAM; leaving `infra.netbox.url` empty leaves the integration unwired.

## Notes on the sharp edges

**The worker's workspace volume must be `ReadWriteMany` with more than one replica.** It holds the
per-cluster OpenTofu state, and any replica may pick up any cluster's next phase, so they all need to
see it. With `ReadWriteOnce` the Deployment gets pinned to one node and the other replicas stall. (It
is a cache, not the truth - Postgres is - but losing it mid-cluster means re-importing state.)

**The shell sandbox never gets the hypervisor key.** Users can break kubectl into arbitrary bash in
there, so it holds no master key, no database URL and no SSH key - only the bearer token the API
authenticates with. That is why, in `remote` mode, kubectl traffic goes through the separate `socks`
tunnel Deployment (which does hold the key, and which nobody gets a shell in) instead of the sandbox
opening its own tunnel.

**Don't rotate `config.secretKey` casually.** It encrypts every stored kubeconfig and signs every
session; changing it orphans the lot. Left empty, the chart generates one on first install and reads
it back on upgrade so it stays stable. Production would source it from a KMS/Vault and set
`existingSecret` instead - the chart is a demo, and keeping credentials in a Helm release is a
shortcut, flagged as such.

**SSE and WebSockets must not be buffered.** The default ingress annotations turn nginx's buffering
off and raise the timeouts; on another controller, set the equivalent or the Activity tab will look
frozen and the Terminal won't work.

See [`values.yaml`](values.yaml) for the full, commented surface.
