# Deploying with Podman Compose

Podman Compose is the single-machine deployment path - a homelab box or a WSL2 host with KVM. It's
the default `make up` and the topology the rest of this guide assumes.

Everything runs in containers. The **worker** is the only one that touches the hypervisor, so in real
mode it runs host-networked with the libvirt socket mounted; everything else stays on an ordinary
Podman network.

## The compose files

The stack is assembled from overlays so one base file serves every mode:

| File | Adds |
|---|---|
| `compose.yaml` | the base: `web` (portal), `api` (JSON+SSE, always fake providers), `vault` (dev), `postgres` |
| `compose.real.yaml` | the host-networked `worker`, the `shell` and `nodessh` sandboxes; switches the API's seams to the worker/agents |
| `compose.ports.yaml` | publishes the host ports (`:8080`, `:8081`, Vault `:8200`) - single-replica |
| `compose.scale.yaml` | N replicas of each tier behind a load balancer (replaces the port overlay) |

The `make` targets combine them for you:

```bash
make up          # compose.yaml + compose.real.yaml + compose.ports.yaml - real VMs (default)
make up-fake     # compose.yaml + compose.ports.yaml - the API reconciles in-process, fake providers
make up-scale    # compose.yaml + compose.real.yaml + compose.scale.yaml - the scaled stack
```

## The real-mode topology

`make up` brings up:

```
      browser
        │  :8080
   ┌────▼─────┐   /api    ┌──────────┐
   │   web    │──────────▶│   api    │   JSON + SSE; never touches a hypervisor
   │ (nginx)  │           └────┬─────┘
   └──────────┘                │        proxied interactive features
                    ┌──────────┼───────────────┬─────────────┐
              ┌─────▼────┐ ┌───▼────┐   ┌───────▼──────┐ ┌────▼─────┐
              │ postgres │ │ vault  │   │ shell sandbox│ │ nodessh  │
              │  (state) │ │(secrets)│  │ bash+kubectl │ │  ssh     │
              └─────▲────┘ └────────┘   └──────────────┘ └──────────┘
                    │
              ┌─────┴──────┐
              │   worker   │  host-networked · the ONLY container that
              │ reconciler │  reaches libvirt and the cluster VMs
              └─────┬──────┘
                    │ OpenTofu · Ansible · Helm
             libvirt / KVM VMs
```

- **`web`** serves the portal SPA and reverse-proxies `/api/*` to the API (SSE and WebSocket aware).
- **`api`** is the JSON + SSE surface. It never reconciles in real mode (`KAAS_DISABLE_RECONCILER=1`)
  and never touches a hypervisor - it proxies the interactive features (terminal, workloads,
  monitoring, security, audit, tunnel) to the exec agents.
- **`worker`** runs the reconcile loop. It's host-networked so it can SSH to the cluster VMs on their
  bridges and reach the published Postgres; it mounts the libvirt socket, the SSH keys, the golden
  images, and the OpenTofu/Ansible trees.
- **`shell`** and **`nodessh`** are the two [hardened sandboxes](../portal/managing-clusters.md) the
  browser terminal and node-SSH run in - host-networked (the only route to the cluster VMs) but
  stripped of the worker's secrets. They hold different things and are contained differently: the
  shell sandbox holds *no* credentials (a bash escape finds nothing), the nodessh sandbox holds the VM
  key but *no shell* (there's no session to read it from).
- **`vault`** is a single-node HashiCorp Vault on file storage in the `vaultdata` volume, initialised
  and unsealed by its own entrypoint (see [Vault](integrations/vault.md)).
- **`postgres`** is the single source of truth. It's published on `:5432` so the host-networked worker
  can reach it.

Why this split: only the worker (and the two host-networked sandboxes) can reach the cluster subnets,
so the KVM/cluster blast radius is confined to them. See the [libvirt provider
guide](providers/libvirt.md) for the networking detail.

## First real-mode run

### 1. Build a golden image

```bash
make golden-image        # the default head image; make golden-images for the upgrade set
```

See [Golden images](golden-images.md).

### 2. Create a `.env`

Copy [`.env.example`](../../.env.example) to `.env` and fill in the real knobs. Compose auto-loads
`.env`, and its values must be **literal** - compose does not run `$(...)` or expand `~`. The bare
essentials for local KVM:

```bash
KAAS_SECRET_KEY=<a long, stable, random string>        # so encrypted secrets survive restarts
KAAS_IMAGE_DIR=/var/lib/libvirt/images                 # per-(OS,k8s) golden images
KAAS_BASE_IMAGE=/var/lib/libvirt/images/ubuntu-26.04-k8s-1.36.2.qcow2   # fallback single image
KAAS_SSH_PUBLIC_KEY=ssh-ed25519 AAAA... kaas           # literal key contents, injected via cloud-init
KAAS_SSH_PRIVATE_KEY_FILE=/abs/path/to/your/private/key   # mounted read-only into the worker
```

For anything shared (real Vault, the exec-agent tokens with more than one host), also set
`KAAS_SHELL_TOKEN` and `KAAS_NODE_SSH_TOKEN`. Every variable is explained in the [configuration
reference](configuration.md).

### 3. Bring it up

```bash
make up          # build + start the real stack
make logs        # follow logs (incl. the worker)
make ps
```

Open **http://localhost:8080**, sign in as `admin` / `admin`, grant your account quota on the
Administration page, and create a cluster. The API is also on **:8081** for direct curl.

### 4. Tear down

```bash
make down        # delete running clusters (waits for VMs to go), then stop every container and prune volumes
```

`make down` deletes clusters *before* stopping the containers, so the worker is never killed
mid-destroy (which would orphan libvirt domains).

**`make down` is a full cleanup**, `pgdata` included — it ends with `podman volume prune`, so the
next `make up` starts from an empty database. The prune runs *last*, after every container is gone
(Harbor's too): a volume is pruneable only while nothing references it, so pruning earlier would
leave the anonymous volumes Harbor's images declare dangling on the host.

The one thing that survives is Harbor's images, and not by exemption — they live in `harbor.yml`'s
`data_volume`, a host directory no volume prune can reach, so the pull-through cache is warm again on
the next `make up`. `make harbor-purge` discards those, and even that leaves `data_volume` itself for
you to remove by hand: re-pulling a warm cache is hours of bandwidth, so nothing does it implicitly.

## Scaling out

Every stateless tier scales; only Postgres is single (see
[Architecture](../concepts/architecture.md#horizontal-scaling)):

```bash
make up-scale                          # 2 × web, 2 × api, 2 × worker, 2 exec agents, 1 postgres
make up-scale WEB=3 API=3 WORKER=4     # pick your own counts
make ps-scale
make down-scale
```

The addresses don't change - an `lb` container (nginx) now owns `:8080`/`:8081` and fans out over the
replicas by DNS, because a published host port can only belong to one container. The exec-agent
sandboxes are host-networked, so each needs its own port on a given host; the scaled compose declares
them as separate services, and across hosts you'd run one agent per host listed in
`KAAS_SHELL_AGENT_ADDR` / `KAAS_NODE_SSH_AGENT_ADDR`.

## Upgrading a running deployment

The stack is meant to run for a long time and take new versions in place. `make rebuild` (rebuild
images + recreate) must never break the clusters it's managing, and it doesn't: the two persistent
volumes (`pgdata`, `workdata`) survive recreation, and the toolchain, provider plugins, and app
binaries all come from the images you're rebuilding. Provider plugin versions are governed by the
image (a baked filesystem mirror, so `init` never contacts a registry), and each workspace re-resolves
them on the next reconcile - so a provider upgrade is safe. The one thing you must **not** change is
`KAAS_SECRET_KEY`.

## Where to next

- Your provider's specifics: [libvirt](providers/libvirt.md), [vSphere](providers/vsphere.md),
  [Proxmox](providers/proxmox.md).
- The optional integrations: [directory auth](integrations/directory-auth.md),
  [DNS](integrations/dns.md), [Vault](integrations/vault.md), [NetBox](integrations/netbox.md).
- Everything you can configure: the [configuration reference](configuration.md).
