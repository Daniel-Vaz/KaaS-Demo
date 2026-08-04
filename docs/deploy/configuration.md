# Configuration reference

Every KubeHarbor setting is an environment variable, prefixed `KAAS_`. This page groups them by area.
Compose reads them from `.env` (see [`.env.example`](../../.env.example) for an annotated copy); the
Helm chart maps them from `values.yaml`.

Two rules worth stating up front:

- **The API never gets provisioning credentials.** vCenter / Proxmox / NetBox / DNS / hypervisor
  secrets go to the **worker only** - the API does admission (naming, quota), not provisioning. The
  compose files enforce this split.
- **Keep `KAAS_SECRET_KEY` stable.** It derives the AES key that encrypts stored secrets *and* the
  session-cookie signing key. Rotating it orphans every stored kubeconfig and invalidates every
  session.

## Platform core

| Var | Default | Meaning |
|---|---|---|
| `KAAS_ADDR` | `:8080` | API listen address inside the container (the portal proxies to it) |
| `KAAS_SECRET_KEY` | ephemeral | Source of the AES-256 secret key **and** the session-cookie signing key. Set a stable value |
| `DATABASE_URL` | unset | Postgres DSN. Unset → in-memory store + tick loop; set → Postgres + River durable queue |
| `KAAS_MIGRATIONS_DIR` | `migrations` | Application migrations directory |
| `KAAS_DISABLE_RECONCILER` | unset | `1` = this process does not reconcile (set on the API in real mode) |
| `KAAS_SEED_DEMO` | unset | `1` = the worker seeds one demo cluster (owned by admin) and logs it to Ready |
| `KAAS_ADMIN_USERNAME` | `admin` | Seeded admin account. Under directory auth it's the **break-glass** account and the directory can never claim the name |
| `KAAS_ADMIN_PASSWORD` | `admin` (warned) | Password for the seeded admin - set it for anything real |
| `KAAS_WORK_DIR` | `$TMPDIR/kaas-workspaces` | Base for per-cluster OpenTofu/Ansible workspaces |

### Capacity budget & quota

Each infrastructure has its own ceiling and its own conserved pool - capacity is never fungible between
backends. See [account & quota](../portal/account-and-admin.md).

| Var | Default | Meaning |
|---|---|---|
| `KAAS_BUDGET_VCPU` | `16` | The **KVM host's** vCPU ceiling, handed out to accounts as per-user KVM quota |
| `KAAS_BUDGET_MEM_MB` | `24576` | The KVM host's memory ceiling (MB) |
| `KAAS_BUDGET_DISK_GB` | `500` | The KVM host's storage ceiling (GB) - charged for every root disk, every extra disk, and the per-worker storage disk |
| `KAAS_SHARED_QUOTA` | `false` | Disable per-user quota: every account draws from each backend's **full ceiling**, first-come-first-served (the aggregate cap still prevents oversubscription). The Admin page shows consumption instead of grants |
| `KAAS_BUNDLE_ADDONS_OPTIONAL` | `false` | Let the create wizard **deselect the bundle's own add-ons**. Off, a cluster is born with the whole batteries-included set and an add-on is removed later from the Add-ons tab. Turn it on when the host can't carry all of it - on a laptop-scale KVM host the bundle can outweigh a small cluster's own workers. API-only (it is an admission decision) |

## Authentication

All of these reach the **API only** - the worker never authenticates a user. Full how-to in [Directory
authentication](integrations/directory-auth.md).

| Var | Default | Meaning |
|---|---|---|
| `KAAS_AUTH` | `local` | `local` = local accounts + open self-registration. `ldap` = Active Directory / LDAP |
| `KAAS_LDAP` | `real` | Orthogonal to `KAAS_AUTH`: `real` talks to the DCs, `fake` is an in-memory directory from the same rules |
| `KAAS_LDAP_CONFIG` | `/etc/kaas/ldap.yaml` | Path to the mapping config **inside the container** |
| `KAAS_LDAP_CONFIG_HOST` | `./ldap.example.yaml` | Compose only: which **host** file to mount there |
| `KAAS_LDAP_BIND_PASSWORD` | unset | The service account's password (a secret, deliberately not in the config file) |
| `KAAS_LDAP_MAX_FAILURES` | `3` | Failed logins **per username** before login is throttled. Keep **below your AD lockout threshold** |
| `KAAS_LDAP_MAX_IP_FAILURES` | `30` | Failed logins **per source address** (higher - an office shares one NAT egress) |
| `KAAS_LDAP_THROTTLE_WINDOW` | `5m` | How long both counters live |

## Backend selection (the fake/real seams)

Each seam picks a fake or real implementation. See
[Architecture](../concepts/architecture.md#the-seams-fake-vs-real).

| Var | Default | Values |
|---|---|---|
| `KAAS_PROVISIONER` | `fake` | `fake` \| `tofu` |
| `KAAS_CONFIG` | `fake` | `fake` \| `ansible` |
| `KAAS_ADDONS` | `fake` | `fake` \| `helm` |
| `KAAS_METRICS` | `fake` | `fake` \| `kubectl` - per-node usage telemetry (worker only) |
| `KAAS_HEALTH` | `fake` | `fake` \| `kubectl` - cluster-health checks (worker only) |
| `KAAS_SHELL` | `fake` | `fake` \| `worker` - the portal's Terminal tab |
| `KAAS_NODE_SSH` | `fake` | `fake` \| `agent` - the Nodes tab's SSH button |
| `KAAS_KUBE` | `fake` | `fake` \| `worker` - the Workloads, Storage, and Networking pages |
| `KAAS_MONITORING` | `fake` | `fake` \| `worker` - the Monitoring page (PromQL) |
| `KAAS_SECURITY` | `fake` | `fake` \| `worker` - the Security page (Trivy CRDs) |
| `KAAS_AUDIT` | `fake` | `fake` \| `worker` - the cluster Audit tab (apiserver logs) |
| `KAAS_TUNNEL` | `fake` | `fake` \| `worker` - the "Open UI" links to in-cluster Grafana/etc. |
| `KAAS_VAULT` | `fake` | `fake` \| `real` - the secret store ([Vault](integrations/vault.md)) |
| `KAAS_DNS` | `fake` | `fake` \| `nsupdate` \| `winrm` - [cluster DNS](integrations/dns.md) (worker only) |
| `KAAS_ADDON_VALUES` | `auto` | `auto` \| `helm` \| `fake` - source of chart `values.yaml` for the in-browser editor |

### The interactive exec agents

The Terminal, Workloads, Monitoring, Security, Audit, and tunnel features are proxied to a
host-networked exec agent - the `shell` sandbox - because the API can't reach cluster API servers.

| Var | Default | Meaning |
|---|---|---|
| `KAAS_SHELL_AGENT_ADDR` | `host.containers.internal:8082` | (API) where to reach the exec agent. A comma-separated list is round-robined and failed over |
| `KAAS_SHELL_LISTEN` | unset | (sandbox) the exec agent's bind address |
| `KAAS_SHELL_TOKEN` | derived | Shared bearer token. The sandbox holds no `KAAS_SECRET_KEY`, so **set an explicit shared value** for containerized real mode |
| `KAAS_SHELL_BIN` | `bash` | The shell the PTY runs |
| `KAAS_KUBECTL_BIN` | `kubectl` | The kubectl binary the agents and collectors use |

Node SSH runs in a **separate** sandbox (it holds the VM key and must not offer a shell):

| Var | Default | Meaning |
|---|---|---|
| `KAAS_NODE_SSH_AGENT_ADDR` | `host.containers.internal:8084` | (API) where to reach the node-ssh sandbox (comma-separated, failed over) |
| `KAAS_NODE_SSH_LISTEN` | `:8084` | (sandbox) bind address |
| `KAAS_NODE_SSH_TOKEN` | derived | Bearer token, **distinct** from `KAAS_SHELL_TOKEN` on purpose |
| `KAAS_SSH_USER` | `kaas` | The VM login (the passwordless-sudo cloud-init account) |
| `KAAS_SSH_PRIVATE_KEY_FILE` | - | (sandbox) the cluster-VM key; required in `agent` mode |

| Var | Default | Meaning |
|---|---|---|
| `KAAS_USER_KUBECONFIG_TTL` | `720h` | Validity of a downloaded per-user kubeconfig (~1 month). Each download mints a fresh cluster-CA-signed cert carrying the actor's identity + role |

## Infrastructure providers

| Var | Default | Meaning |
|---|---|---|
| `KAAS_INFRA_PROVIDERS` | `kvm` | Comma-separated, ordered; the first is the default. `kvm` \| `vsphere` \| `proxmox`. More than one adds the wizard's Infrastructure step. Orthogonal to `KAAS_PROVISIONER` |

Provider-specific settings live with their guides - the tables below are the essentials.

### libvirt / KVM provisioner

Full guide: [libvirt](providers/libvirt.md).

| Var | Default | Meaning |
|---|---|---|
| `KAAS_IMAGE_DIR` | unset | Directory of per-(OS,k8s) golden images `<os>-k8s-<v>.qcow2` |
| `KAAS_BASE_IMAGE` | required | Fallback base/golden qcow2 for versions not in `KAAS_IMAGE_DIR` |
| `KAAS_SSH_PUBLIC_KEY` | required | SSH public key injected via cloud-init (literal contents) |
| `KAAS_TOFU_BIN` | `tofu` | OpenTofu binary |
| `KAAS_LIBVIRT_MODULE_DIR` | `infra/libvirt` | The module copied into each workspace |
| `KAAS_LIBVIRT_URI` | `qemu:///system` (or derived from `KAAS_KVM_HOST`) | libvirt connection URI |
| `KAAS_LIBVIRT_POOL` | `default` | libvirt storage pool |
| `KAAS_NET_SUPERNET` | `10.200.0.0/16` | Supernet carved into per-cluster networks |
| `KAAS_NET_PREFIX` | `24` | Prefix length of an auto-allocated cluster network |
| `KAAS_NET_RESERVED` | pod/svc/podman/libvirt CIDRs | Ranges a cluster CIDR must never overlap |

#### Remote KVM host

Unset `KAAS_KVM_HOST` means the local hypervisor. Set it and the platform provisions onto another
machine's libvirt over SSH - see [libvirt § remote hosts](providers/libvirt.md#remote-kvm-hosts).

| Var | Default | Meaning |
|---|---|---|
| `KAAS_KVM_HOST` | unset (local) | Hostname/IP of the KVM host - the one switch; everything below only matters when it's set |
| `KAAS_KVM_SSH_USER` | `root` | SSH user (must be able to use libvirtd) |
| `KAAS_KVM_SSH_PORT` | `22` | SSH port |
| `KAAS_KVM_SSH_KEY_FILE` | required when remote | The platform's key to the KVM host (not the cluster-VM key) |
| `KAAS_KVM_KNOWN_HOSTS_FILE` | unset | `known_hosts` to verify against; unset disables host-key checking (dev shortcut) |
| `KAAS_KVM_SOCKS_ADDR` | `127.0.0.1:1080` | Where the worker binds the SOCKS tunnel (shared by the shell sandbox) |
| `KAAS_RECONCILE_JOB_TIMEOUT` | `15m` | Per-reconcile-phase kill budget - **almost always raise it** for a remote host (image staging) |

### vSphere

Full guide: [vSphere](providers/vsphere.md). Worker-only credentials.

| Var | Default | Meaning |
|---|---|---|
| `KAAS_VSPHERE_URL` / `_USERNAME` / `_PASSWORD` | required | vCenter endpoint and credentials |
| `KAAS_VSPHERE_INSECURE` | `0` | `1` accepts a self-signed vCenter cert |
| `KAAS_VSPHERE_DATACENTER` / `_CLUSTER` / `_DATASTORE` / `_FOLDER` | required | Placement. **Choose the datastore for latency** (etcd WAL) |
| `KAAS_VSPHERE_MODULE_DIR` | `infra/vsphere` | OpenTofu module |
| `KAAS_VSPHERE_NETWORK` | required | The shared portgroup |
| `KAAS_VSPHERE_NET_MODE` | `dhcp` | `dhcp` \| `static` |
| `KAAS_VSPHERE_NET_CIDR` | required | The portgroup's subnet |
| `KAAS_VSPHERE_NET_GATEWAY` / `_NET_DNS` / `_NET_RANGE` | static only | Gateway, resolvers, allocation range |
| `KAAS_VSPHERE_BUDGET_VCPU` / `_MEM_MB` / `_DISK_GB` | `64` / `131072` / `4096` | The vSphere capacity ceiling |

### Proxmox VE

Full guide: [Proxmox](providers/proxmox.md). Worker-only credentials; set **either** a token **or** a
username/password.

| Var | Default | Meaning |
|---|---|---|
| `KAAS_PROXMOX_ENDPOINT` | required | Proxmox API base, e.g. `https://host:8006/` |
| `KAAS_PROXMOX_INSECURE` | `0` | `1` accepts a self-signed cert |
| `KAAS_PROXMOX_API_TOKEN` | token auth | `user@realm!tokenid=secret` |
| `KAAS_PROXMOX_USERNAME` / `_PASSWORD` | password auth | The fallback to a token |
| `KAAS_PROXMOX_NODE` / `_DATASTORE` | required | The node VMs are created on; the datastore for their disks |
| `KAAS_PROXMOX_MODULE_DIR` | `infra/proxmox` | OpenTofu module |
| `KAAS_PROXMOX_NET_BRIDGE` | required | The shared bridge |
| `KAAS_PROXMOX_NET_VLAN` | `0` | VLAN tag on a VLAN-aware bridge (0 = untagged) |
| `KAAS_PROXMOX_NET_MODE` | `dhcp` | `dhcp` \| `static` |
| `KAAS_PROXMOX_NET_CIDR` | required | The bridge's subnet |
| `KAAS_PROXMOX_NET_GATEWAY` / `_NET_DNS` / `_NET_RANGE` | static only | Gateway, resolvers, allocation range |
| `KAAS_PROXMOX_BUILD_IP` | golden-image build only | A free address for the Packer build VM on a static-only network |
| `KAAS_PROXMOX_BUDGET_VCPU` / `_MEM_MB` / `_DISK_GB` | `64` / `131072` / `4096` | The Proxmox capacity ceiling |

## Integrations

### NetBox IPAM (optional; shared-network providers)

Full guide: [NetBox](integrations/netbox.md).

| Var | Default | Meaning |
|---|---|---|
| `KAAS_NETBOX_URL` | unset | Unset = not wired |
| `KAAS_NETBOX_TOKEN` / `_USERNAME` / `_PASSWORD` | unset | API token, or a login to mint one |
| `KAAS_NETBOX_INSECURE` | `0` | `1` accepts a self-signed cert |
| `KAAS_NETBOX_TAG` | `kaas` | The tag our records carry |

### Cluster DNS (optional)

Full guide: [DNS](integrations/dns.md). `KAAS_DNS_BASE_DOMAIN` is read by both processes (naming); the
server and credentials are worker-only.

| Var | Default | Meaning |
|---|---|---|
| `KAAS_DNS_BASE_DOMAIN` | unset | Unset = no cluster DNS. Every cluster owns `<cluster>.<base>` |
| `KAAS_DNS_APPS_LABEL` | `apps` | The wildcard is `*.<label>.<cluster>.<base>` |
| `KAAS_DNS` | `fake` | `fake` \| `nsupdate` (RFC 2136) \| `winrm` (Windows DNS Server) - worker only |
| `KAAS_DNS_SERVER` | unset | The DNS server (use its FQDN for GSS-TSIG) |
| `KAAS_DNS_ZONE` | the base domain | The delegated zone that holds the records |
| `KAAS_DNS_TTL` | `300` | Record TTL |
| `KAAS_DNS_AUTH` | `gss` | `gss` (AD secure updates) \| `tsig` (shared key) \| `none` |
| `KAAS_DNS_KRB_USERNAME` / `_PASSWORD` / `_REALM` | unset | The service account allowed to write the zone (GSS) |
| `KAAS_DNS_TSIG_KEYNAME` / `_SECRET` / `_ALGO` | unset / - / `hmac-sha256` | TSIG credential |
| `KAAS_WINRM_HOST` / `_PORT` / `_USERNAME` / `_PASSWORD` / `_INSECURE*` | unset | `KAAS_DNS=winrm` only - drives Windows DNS Server over WinRM |

### HashiCorp Vault (optional)

Full guide: [Vault](integrations/vault.md).

| Var | Default | Meaning |
|---|---|---|
| `KAAS_VAULT` | `fake` | `fake` \| `real` |
| `KAAS_VAULT_ADDR` | `http://vault:8200` | Vault API address - the platform's own route (API + worker) |
| `KAAS_VAULT_CLUSTER_ADDR` | = `KAAS_VAULT_ADDR` | Address written into each cluster's ClusterSecretStore; ESO must reach it from inside the cluster |
| `KAAS_VAULT_TOKEN` | - | The worker's management token, or the API's narrow minter token |
| `KAAS_VAULT_MOUNT` | `kaas` | The KV v2 mount the platform owns |
| `KAAS_VAULT_UI_URL` | = `KAAS_VAULT_ADDR` | Browser-facing base for the "View in Vault" deep-link |
| `KAAS_VAULT_INSECURE` | `0` | `1` accepts a self-signed Vault cert |
| `KAAS_VAULT_TOKEN_TTL` | - | Bounds a minted handoff token's validity |

## Self-healing

All worker-only, all on by default. Behaviour is described in [Keeping clusters
healthy](../concepts/keeping-clusters-healthy.md).

### Certificate rotation

| Var | Default | Meaning |
|---|---|---|
| `KAAS_CERT_RENEW` | `1` | Automatic control-plane certificate rotation; `0` disables |
| `KAAS_CERT_RENEW_WINDOW` | `720h` | How close to expiry rotation fires (30 days) |

### etcd maintenance

| Var | Default | Meaning |
|---|---|---|
| `KAAS_ETCD_MAINTENANCE` | `1` | Automatic defragmentation; `0` disables |
| `KAAS_ETCD_OBSERVE_INTERVAL` | `6h` | How often etcd's store is re-read |
| `KAAS_ETCD_DEFRAG_RATIO` | `0.45` | Fragmentation share worth the stop-the-world cost |
| `KAAS_ETCD_DEFRAG_MIN_BYTES` | `104857600` | Absolute size floor below which fragmentation is ignored (100 MiB) |
| `KAAS_ETCD_DEFRAG_MIN_INTERVAL` | `24h` | Hysteresis floor between defrags of one cluster |
| `KAAS_ETCD_QUOTA_BYTES` | `8589934592` | `--quota-backend-bytes` baked into every cluster (8 GiB; clamped to [2 GiB, 8 GiB]) |
| `KAAS_ETCD_COMPACTION_RETENTION` | `1h` | etcd-side auto-compaction retention |
| `KAAS_MAINTENANCE_WINDOW` | *(any time)* | When disruptive maintenance may run: `HH:MM-HH:MM`, `Sun 02:00-06:00`, etc. An armed `NOSPACE` bypasses it |
| `KAAS_MAINTENANCE_TZ` | `UTC` | IANA timezone for the window |

### Control-plane backups

| Var | Default | Meaning |
|---|---|---|
| `KAAS_ETCD_SNAPSHOT` | `1` | Periodic sealed etcd snapshots; `0` disables (and with it sole-CP recovery) |
| `KAAS_ETCD_SNAPSHOT_INTERVAL` | `6h` | Backup cadence - also the bound on how much an automatic restore loses |
| `KAAS_ETCD_SNAPSHOT_RETAIN` | `3` | Snapshots kept per cluster (clamped ≥ 1) |
| `KAAS_ETCD_SNAPSHOT_MAX_RESTORE_AGE` | `24h` | How stale a snapshot may be and still be restored automatically |

### Automatic repair

| Var | Default | Meaning |
|---|---|---|
| `KAAS_REPAIR` | `1` | Automatic cluster/node repair; `0` disables |
| `KAAS_REPAIR_REPLACE` | `1` | May the ladder **rebuild a node** |
| `KAAS_REPAIR_RESTORE` | `1` | May the ladder **restore a sole control plane** from a snapshot |
| `KAAS_REPAIR_INTERVAL` | `2m` | How often repair state is refreshed |
| `KAAS_REPAIR_HEALTH_MAX_AGE` | `1m` | How stale a health snapshot may be and still be believed |
| `KAAS_REPAIR_NOTREADY_GRACE` | `10m` | How long a node stays NotReady before the first repair |
| `KAAS_REPAIR_STARTUP_GRACE` | `20m` | How long a joining node may lack a node object before it counts as faulty |
| `KAAS_REPAIR_REPLACE_AFTER` | `30m` | How long a fault persists across cheaper attempts before a rebuild |
| `KAAS_REPAIR_MAX_UNHEALTHY` | `0.5` | Per-cluster blast-radius cap (needs ≥ 2 faults to trip) |
| `KAAS_REPAIR_MAX_UNHEALTHY_CLUSTERS` | `0.5` | Per-fleet blast-radius cap |
| `KAAS_REPAIR_MAX_ATTEMPTS` | `3` | Attempts before a node is suspended |
| `KAAS_REPAIR_BACKOFF` | `30m` | Base delay between attempts (doubling, capped at 24h) |

## Real backend binaries

| Var | Default | Meaning |
|---|---|---|
| `KAAS_ANSIBLE_BIN` | `ansible-playbook` | Ansible binary |
| `KAAS_ANSIBLE_DIR` | `ansible` | Playbooks/roles directory |
| `KAAS_HELM_BIN` | `helm` | Helm binary |

## t-shirt sizes

Per-node resources are fixed by size (`internal/domain`):

| Size | vCPU | Memory | Disk |
|---|---|---|---|
| `small` | 2 | 8192 MB | 40 GB |
| `medium` | 4 | 16384 MB | 80 GB |
| `large` | 8 | 32768 MB | 160 GB |

Every node uses its size's resources, so an HA `small` cluster with 2 workers is 5 × (2 vCPU / 8 GB).

## `make` target reference

| Target | What it does |
|---|---|
| `make up` / `down` / `nuke` | real-mode stack up / down (deletes clusters first) / down + wipe DB volume |
| `make up-fake` / `down-fake` | fake-mode stack (no worker) |
| `make up-scale` / `down-scale` | scaled real-mode stack (`WEB=3 API=3 WORKER=4` to override) |
| `make logs` / `ps` (+ `-fake` / `-scale`) | follow logs / status |
| `make restart` / `rebuild` | bounce / rebuild images and recreate |
| `make psql` | psql shell into Postgres |
| `make kubeconfig CLUSTER=<id>` | fetch a cluster's admin kubeconfig to `/tmp/kubeconfig` |
| `make golden-image` / `golden-images` | build the Packer golden image(s) |
| `make golden-image-vsphere` / `golden-image-proxmox` | build the provider templates |
| `make helm-lint` / `helm-template` | lint / render the Helm chart |
| `make images` / `images-push` | build (and push) the chart's images |
| `make catalog-check` / `catalog-update` | report / apply newer upstream add-on chart versions |
| `make web-install` / `web-dev` / `web-build` | portal deps / Vite dev server / production build |
| `make build` / `test` / `vet` | Go build / unit tests / vet |
| `make run-api` / `run-worker` | run a component locally (no containers) |
