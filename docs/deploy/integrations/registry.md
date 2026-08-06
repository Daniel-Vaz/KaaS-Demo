# Container image registry (Harbor)

KubeHarbor can run a container image registry beside the platform: **one Harbor**, shared by every
cluster, with a **private project per cluster** whose membership mirrors the portal's own read/write
model, and **pull-through caches** of the public registries every cluster pulls from.

It is optional, and enabled by **configuring** it: create `deploy/harbor/harbor.yml` and every
`make up` brings Harbor up with the platform. Without that file the portal's Registry page renders
simulated data and nothing about cluster bring-up changes.

- Seam: `internal/registry` (`KAAS_REGISTRY=fake|real`), real implementation `internal/registry/harbor`
- Portal: the **Registry** page, and one line on each cluster's Overview
- Configuration: [configuration reference](../configuration.md)

---

## What it gives you

**A place to push.** Every cluster gets a private project, `kaas-<cluster name>`, and a push/pull
robot account. The credential is applied inside the cluster as the `kaas-registry` imagePullSecret in
the `default` namespace and written to the cluster's Vault path, so External Secrets can sync it into
any other namespace. Nothing has to be configured by hand:

```
docker login <registry host>
docker tag myapp:latest <registry host>/kaas-dev/myapp:latest
docker push <registry host>/kaas-dev/myapp:latest
```

**A pull-through cache.** With `KAAS_REGISTRY_MIRROR=1` (the default when a registry is configured)
the platform creates a proxy-cache project per upstream — `docker.io`, `ghcr.io`, `quay.io`,
`registry.k8s.io` — and configures every cluster node's containerd to resolve those registries
through it. The first cluster to install an add-on populates the cache; every cluster after that
pulls over the LAN.

Each cache is independent, and a failure to create one is logged and stepped over rather than
returned: every `hosts.toml` keeps the upstream as its `server` fallback, so an upstream Harbor
cannot proxy costs pull speed for that one registry and nothing else. (The provider type each
upstream declares must be a name Harbor's own `GET /api/v2.0/replication/adapters` lists — an unknown
one is rejected as an opaque `500`.)

**Access that matches the portal.** A user sees, and can push to, exactly the projects the portal
would let them manage:

| Relationship to the cluster | Harbor project role |
|---|---|
| Owner | Project Admin |
| Platform admin | Harbor system admin (sees everything) |
| Group-mate, **write** role | Developer — pull + push |
| Group-mate, **read** role | Guest — pull only |

This is converged by a leader-elected sweep (`registry.Manager.SyncAccess`), because membership edits
are API-side writes that never bump a cluster's generation. The **same function** that computes it
filters the Registry page, so what the page shows and what the registry permits cannot drift.

---

## Setting it up

### 1. Bring Harbor up

```bash
cp deploy/harbor/harbor.yml.example deploy/harbor/harbor.yml
$EDITOR deploy/harbor/harbor.yml      # set `hostname` - see below
```

Two settings name **host directories** Harbor bind-mounts — `data_volume` and `log.local.location`.
Podman, unlike docker, will not create a missing bind-mount source, so `scripts/harbor.sh` creates
both before starting; upstream's defaults (`/data`, `/var/log/harbor`) need root, and when it cannot
create one it prints the `sudo mkdir` to run. Pointing both somewhere you already own avoids that.
`data_volume` is the one that grows — it holds every cached and pushed image.

**That file is the switch.** Once it exists, every `make up` (and `make up-fake`) brings Harbor up
alongside the platform and points the API and worker at it. Without it nothing changes: the registry
seam stays `fake` and no cluster is configured to pull through anything. `make harbor-up` brings
Harbor up on its own if you want it before the platform.

Harbor runs from **its own installer** (`scripts/harbor.sh` drives `prepare`), which generates its
compose file and internal configuration from `harbor.yml`. The repo deliberately does not vendor
Harbor's eight services: that would be a fork of somebody else's deployment, re-derived on every
Harbor release. For the same reason `harbor.yml.example` is upstream's own template with our values
changed, not a hand-written subset - Harbor validates the whole schema and a partial file fails.

Harbor's installer assumes docker, and this platform runs podman, so `harbor.sh` makes two
adjustments around it. Both apply to **generated output**, so `prepare` re-generating it changes
nothing:

- It creates the bind-mount source directories itself. Docker creates a missing one; podman refuses,
  mid-startup, with a bare `statfs ...: no such file or directory`.
- It strips the **`syslog` log driver** from the generated compose file. Harbor points every service
  at its `harbor-log` sidecar over syslog, and podman implements no such driver — every service but
  the sidecar dies at create time with `invalid log driver: invalid argument`, buried under a cascade
  of dependency errors. Without the block, podman uses its own default and `podman logs harbor-core`
  works. Under docker the sidecar is left exactly as Harbor intended.

Concurrent invocations are serialized with an `flock`, because `ensure` runs on every `make up` and
Harbor takes minutes to start: a second `make up` inside that window would otherwise fork a competing
`prepare` + `compose up` onto the same container names and wedge both.

Harbor's lifecycle follows the platform's: `make up` starts it, `make down` tears it down (likewise
`up-fake`/`down-fake` and `up-scale`/`down-scale`).

`make down` is a **full cleanup**, and that includes Harbor's state. Everything Harbor is stateful
about — the registry blobs, its own Postgres database (projects, robots, memberships), redis, its
generated secrets — lives under `harbor.yml`'s `data_volume`, a **host directory**. Nothing there is
a podman volume, so `podman volume prune` cannot reach it: stopping the containers alone would leave
the previous deployment's projects and cached images in place for the next `make up` to find.

That costs you the warm cache, which is the integration's main payoff — so there are two ways to keep
it:

| Command | Containers | `data_volume` |
|---|---|---|
| `make down` | removed | **deleted** |
| `make down KEEP_CACHE=1` | removed | kept |
| `make harbor-down` | removed | kept |
| `make harbor-purge` | removed | **deleted** |

The `goharbor/*` container images are never removed by any of these (`podman rmi` if you want the
~2 GB back; the next `make up` re-pulls them).

Deleting `data_volume` needs `podman unshare`, and `harbor.sh` does it for you: under rootless podman
the `database` and `redis` directories are owned by a **subuid** at mode `0700`, so a plain `rm -rf`
as your own user fails part-way through and leaves behind a half-deleted Harbor that no longer
starts.

> **`hostname` is the setting to get right.** Harbor bakes it into the tokens it issues and the URLs
> it emits, and it must be an address a **cluster node** can reach — a LAN address, never
> `localhost`. Getting it wrong produces the failure that looks like "pulls work from my machine and
> fail on every cluster."

### 2. Point the platform at it

```bash
# .env
KAAS_REGISTRY_HOST=192.168.1.20:8090      # what CLUSTER NODES dial
KAAS_REGISTRY_PASSWORD=<the harbor admin password>
```

```bash
make up                 # Harbor comes up with it
```

On Kubernetes, set the `registry.*` values (and optionally `registry.harbor.enabled=true` to deploy
Harbor as a chart dependency) — see [the Helm guide](../helm.md) and `deploy/helm/kaas/values.yaml`.

### 3. Optional: warm the cache

```bash
make registry-warm
```

This renders the current bundle's add-on charts with `helm template`, extracts every image they
reference, and pulls each once **through** the cache. The list is derived from the catalog, never
curated, so a version bump needs no edit. It closes the one gap the cache leaves — the first cluster,
which would otherwise pay a cache-miss penalty on top of the download it was going to do anyway.

---

## The two addresses

This trips people up, so it gets its own section.

| Setting | Who uses it | Example |
|---|---|---|
| `KAAS_REGISTRY_URL` | the API and worker, talking to Harbor's API | `http://localhost:8090` |
| `KAAS_REGISTRY_HOST` | **cluster nodes**, in image references and containerd config | `192.168.1.20:8090` |

They are separate for the same reason `KAAS_VAULT_ADDR` and `KAAS_VAULT_CLUSTER_ADDR` are, and it
bites harder here: this value is baked into every image reference and must appear in the TLS
certificate's SANs. Left empty, `KAAS_REGISTRY_HOST` falls back to the URL's host — correct only when
both genuinely share one route.

---

## TLS

The lab default is plain HTTP (`KAAS_REGISTRY_INSECURE=1`). For anything else:

1. Give Harbor a certificate whose SANs cover `KAAS_REGISTRY_HOST` (see the `https:` block in
   `harbor.yml.example`).
2. Put the signing CA somewhere the worker can read it and set `KAAS_REGISTRY_CA_FILE`.
3. Drop `KAAS_REGISTRY_INSECURE`.

The worker reads the CA once at start-up and hands it to every cluster node, where the
`registry_trust` Ansible role installs it into the system trust store and into containerd's
`certs.d`. Nothing else needs configuring — including on nodes created later.

---

## How it reaches cluster nodes

Registry trust and the mirror configuration are **not** reconcile-loop work. A node's first image
pull happens during bring-up, long before the cluster is Ready, so configuration that arrived at
Ready would arrive after every pull it was meant to accelerate.

Instead the `registry_trust` role runs from the `common` role, which every path that brings a node
into service already runs — bootstrap, worker join, control-plane join, rejoin, restore. It is a hard
no-op when no registry is configured. It writes, per upstream:

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://docker.io"                     # the FALLBACK, so an outage costs speed, not bring-up

[host."https://harbor.lab/v2/kaas-cache-dockerhub"]
  capabilities = ["pull", "resolve"]
  override_path = true                           # Harbor's cache lives under a project path
  ca = "/etc/containerd/certs.d/kaas-registry-ca.crt"
```

**It carries no credential**, and that is deliberate. The obvious design — a fleet-wide pull-only
robot in every node's `hosts.toml` — fails three ways: containerd 2.x removed static registry auth
from `config.toml`, so the secret would have to sit in a Basic authorization header in a file on
every tenant's VM; a Harbor robot's secret is returned only at creation, so replicas that each
re-minted it would invalidate each other's copy; and it puts a standing fleet credential on every
tenant's node for no gain. The cache projects are **public** instead, which is honest about what they
hold — public upstream images anyone could pull from the upstream anyway. Private images use a
different mechanism entirely: the cluster's own project, reached with the per-cluster robot delivered
as an ordinary Kubernetes imagePullSecret.

A second benefit falls out: with no minted secret in it, the node configuration is a pure function of
the settings plus the CA file, so **every worker replica derives the same thing with no
coordination** — a cluster brought up by a non-leader replica is still configured to pull through the
cache.

---

## Is the cache worth it?

Short answer: yes for the pull-through cache, no for wholesale replication.

**What is actually pulled.** The golden image already bakes the kubeadm control-plane images
(`kubeadm config images pull` runs during the image build), so there is nothing to gain there. What
remains is the add-on wave, and it is re-paid **per node** — and again on every rolling OS upgrade,
every repair that replaces a node, and every pool scale-up:

| Layer | Rough size | Mirrorable |
|---|---|---|
| kubeadm control-plane images | — | already baked into the golden image |
| Cilium (CNI) | ~0.5 GB/node | yes |
| Longhorn | ~1 GB+/node | yes — the largest single item |
| kube-prometheus-stack | ~0.8 GB | yes |
| trivy-operator **and its vulnerability DB** | ~0.3 GB, re-fetched | yes — a reliability win, not just speed |
| metallb, envoy-gateway, cert-manager, external-dns, external-secrets, metrics-server | ~0.3 GB | yes |

Order of magnitude: **1.5–2 GB per node, 3–5 GB per three-node cluster.**

**Why the cache wins over curated replication.** The cache needs no curation — the first cluster
populates it as a side effect of installing. Pre-seeding a hand-maintained image list would mean
deriving it per add-on (the catalog does not carry images), re-deriving it on every version bump, and
storing the whole bundle whether or not anyone selects it. Its only unique benefit — true air-gapped
operation — is not a goal here. `make registry-warm` takes the middle path: derived, never curated,
and run when an operator wants it rather than by the reconcile loop.

**The costs, stated plainly.** The first pull through a cold cache is *slower* than direct — an extra
hop plus a cache write. Harbor itself costs ~1.5–2 GB of RAM and 10–20 GB of disk on the same host
whose capacity the platform's quota is guarding. That is why it is opt-in, and why the platform
configures a weekly garbage collection and per-project quotas from the start rather than letting the
cache grow unbounded.

**Measure it on your hardware.** The figures above are estimates. Time `PhaseInstallingAddons →
PhaseReady` for an identical cluster shape in three conditions — no registry, cold cache, warm cache
— and decide from that. If warm-cache does not clearly beat the baseline, set
`KAAS_REGISTRY_MIRROR=0`: the two halves are independent by construction, so the per-cluster projects
keep working and nothing else changes.

---

## Authentication

The registry follows `KAAS_AUTH`, exactly as Vault does.

**`KAAS_AUTH=local`** → Harbor uses its own user database. The platform is the single writer: it
creates one Harbor account per platform user and turns self-registration off. Platform accounts store
bcrypt hashes, so there is no plaintext password to copy — instead the portal offers **Generate
password**, which mints one, applies it, and shows it to its owner **once**. The platform stores
nothing; re-running rotates.

**`KAAS_AUTH=ldap`** → Harbor authenticates against the same directory, configured from the same
mounted `ldap.yaml` the portal and Vault read. Users sign in with their directory credentials and no
password is ever generated or copied.

Harbor's identity configuration is written by the **API**, at start-up, and not by the worker's
leader-elected registry sweep — because writing it needs the directory settings, and those reach the
API alone (the worker never holds the bind password; it is the process with the libvirt socket and
every tenant's secrets). It is idempotent, so every API replica writing the same thing is harmless,
and a failure is logged rather than fatal.

The worker is still told *which* mode the deployment uses, so it does not create a local Harbor
account for every directory user — but as **`KAAS_REGISTRY_AUTH_MODE`**, a registry-scoped variable
the compose overlay and the chart set for you. Do not hand the worker plain `KAAS_AUTH`: every seam
reads it, and the Vault seam refuses to start on `ldap` without directory settings, so the worker
exits at boot and nothing reconciles at all. A deployment that leaves `KAAS_AUTH` unset is telling the
API nothing about how it authenticates, so the platform leaves Harbor's configuration **untouched**
rather than guessing — that is a deliberate refusal, since guessing wrong locks every user out.

The Registry page reports the mode **Harbor is actually in**, read back from it, not the one this
deployment intends. The two can differ legitimately (an untouched Harbor sits on its own default),
and that difference is exactly what the field is read to find out.

> **Set `KAAS_AUTH` before the first sweep.** Harbor refuses to change `auth_mode` once its own
> database holds any user, because switching would orphan them — and in local mode the platform
> creates one Harbor account per platform user. So a Harbor that ran even briefly in local mode has
> to have those accounts deleted (Administration → Users) before it will accept `ldap`; the platform
> recreates its own. The error says so when it happens, and it is not transient — retrying never
> clears it.

Memberships are converged **per user in both modes**, never by mapping an LDAP group. Harbor can take
a group DN as a project member and it is tempting, but this platform's directory mapping is a list of
arbitrary LDAP filters — several of which may share one `group_key` — and a platform group can mix
directory-derived and locally-added members. There is frequently no single DN that means "this
platform group". Converging per user is one mechanism for both modes and keeps Postgres the single
source of truth.

---

## Deleting a cluster

The cluster's project and robot are released in `PhaseDeleting`, **before** the infrastructure is
destroyed — the same ordering as the DNS record and the Vault path, and here the reason is sharper: a
project is named after the cluster, and cluster names are reusable. A project that outlived its
cluster would be silently inherited by the next cluster of that name, handing one tenant another
tenant's images.

Set `KAAS_REGISTRY_RETAIN_PROJECT=1` to keep the images instead. The robot is deleted either way.

---

## Deliberate lab shortcuts

In the repo's style, stated rather than hidden:

- **Plain HTTP by default.** Production sets `KAAS_REGISTRY_CA_FILE` and a real certificate; the same
  code path then distributes the CA to every node.
- **The API's credential.** It should be a read-only robot. Defaulting it to the admin account is
  what makes the self-service password button work, and it widens what a compromised API replica can
  do. Set `registry.apiUsername`/`apiPassword` (Helm) or `KAAS_REGISTRY_USERNAME` (compose) to a
  robot and the button hides itself.
- **Robots are minted with a long TTL and never rotated.** The expiry is recorded on the cluster row
  (`registry_robot_not_after`) so a rotation sweep can be added as a time-driven due-scan, exactly
  like certificate renewal.
- **Local-mode passwords are generated and displayed.** Production would run an OIDC IdP in front of
  both the portal and Harbor so no password is ever generated, copied or shown.
- **No image signing or admission verification** (cosign, Notation). Harbor's own scan-on-push, which
  the Registry page surfaces per artifact, is as far as this goes.
