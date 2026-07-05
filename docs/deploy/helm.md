# Deploying with Helm on Kubernetes

The `deploy/helm/kaas` chart runs KubeHarbor on an **existing** Kubernetes cluster. It deploys the
portal, the API, the reconciler worker, the shell and node-ssh sandboxes, Vault, and (optionally)
Postgres - every tier with multiple replicas. This is the path that takes advantage of the platform's
horizontal scalability.

```
                    ┌──────────┐
   browser ────────▶│ Ingress  │
                    └────┬─────┘
                  ┌──────▼──────┐        ┌──────────────┐
                  │  web ×N     │───────▶│   api ×N     │   JSON + SSE; no hypervisor, ever
                  └─────────────┘  /api  └───┬──────┬───┘
                                             │      │ terminal / workloads /
                            ┌────────────────▼─┐  ┌─▼──────────────┐
                            │   postgres (1)   │  │   shell ×N     │  bash + kubectl,
                            └────────▲─────────┘  └─────┬──────────┘  no platform secrets
                                     │                  │ kubectl → SOCKS
                             ┌───────┴──────┐   ┌───────▼────────┐
                             │  worker ×N   │──▶│  socks ×N      │  the only pods holding
                             └──────────────┘   └───────┬────────┘  the hypervisor key
                                                        │
                                                  KVM host (VMs)
```

## Try it with no hypervisor

`providers=fake` runs the **same** control plane against simulated backends - the Kubernetes
counterpart of `make up-fake`, and the way to see the chart work on any cluster:

```bash
helm install kaas deploy/helm/kaas --set providers=fake
kubectl port-forward svc/kaas-web 8080:80    # → http://localhost:8080  (admin / admin)
```

## A real install

Real mode provisions actual VMs, so it needs a hypervisor and **two SSH keypairs** - one for the
**hypervisor** and one for the **cluster VMs** the platform creates. They're different keys with
different blast radii; don't confuse them.

```bash
helm install kaas deploy/helm/kaas \
  --set kvm.host=192.168.1.50 \
  --set-file kvm.ssh.privateKey=/path/to/hypervisor_key \
  --set-file config.clusterSSH.privateKey=/path/to/vm_key \
  --set-file config.clusterSSH.publicKey=/path/to/vm_key.pub \
  --set ingress.enabled=true --set ingress.host=kaas.example.com
```

`kvm.mode` is the one choice that shapes everything:

| | `remote` (default) | `local` |
|---|---|---|
| Where the VMs are | another machine, over SSH | the node the pod lands on |
| worker / shell pods | ordinary pods - no host network, no host paths, freely scheduled | `hostNetwork` + the node's libvirt socket as a `hostPath`, pinned by `kvm.local.nodeSelector` |
| shell replicas | any number | one per libvirt node (they all want `:8082` on the host) |
| use it when | always, in Kubernetes | single-node / dev cluster only |

## vSphere and Proxmox

`kvm` is always available; add `vsphere` and/or `proxmox` to `infra.providers` to offer them too. They
need no `hostNetwork`/tunnel dance - the worker reaches vCenter / Proxmox and the cluster VMs directly,
and their OpenTofu modules are baked into the worker image (no volume mount, unlike KVM's libvirt
socket):

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

Same shape for `vsphere` - see the commented `infra.vsphere` / `infra.proxmox` blocks in
[`values.yaml`](../../deploy/helm/kaas/values.yaml). Both are shared-network providers with their own
`budget` (capacity is never fungible between backends). `infra.netbox` optionally records their
addresses in NetBox.

## Sharp edges

- **The worker's workspace volume must be `ReadWriteMany` with more than one replica.** It holds the
  per-cluster OpenTofu state, and any replica may pick up any cluster's next phase, so they all need to
  see it. With `ReadWriteOnce` the Deployment pins to one node and the others stall. (It's a cache, not
  the truth - Postgres is - but losing it mid-cluster means re-importing state.)
- **The shell sandbox never gets the hypervisor key.** Users can break kubectl into arbitrary bash in
  there, so it holds no master key, no database URL, and no SSH key - only the bearer token the API
  authenticates with. That's why, in `remote` mode, kubectl traffic goes through the separate `socks`
  tunnel Deployment (which does hold the key, and which nobody gets a shell in).
- **Don't rotate `config.secretKey` casually.** It encrypts every stored kubeconfig and signs every
  session. Left empty, the chart generates one on first install and reads it back on upgrade so it
  stays stable. Point `config.existingSecret` at your own for production.
- **SSE and WebSockets must not be buffered.** The default ingress annotations turn nginx buffering off
  and raise the timeouts; on another controller set the equivalent, or the Activity tab will look
  frozen and the Terminal won't work.
- **Postgres is not replicated** by the chart. Point `postgres.external.dsn` at a managed instance for
  anything real.

The full, commented surface is in [`values.yaml`](../../deploy/helm/kaas/values.yaml). Lint and render
it in every mode with `make helm-lint`.
