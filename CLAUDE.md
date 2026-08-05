# KaaS-Demo (KubeHarbor)

A **Kubernetes-as-a-Service** control plane, branded **KubeHarbor** ("Kubernetes Without the Rough
Seas"); the codebase, Go module, and env prefix stay `kaas` / `KAAS_`. A web portal and REST API let
users request and manage Kubernetes clusters (name, size, version bundle, add-ons, single-node or HA
control plane). The platform provisions VMs with OpenTofu (libvirt/KVM, vSphere, or Proxmox), forms
them into `kubeadm` clusters with Ansible, installs a CNI and add-ons with Helm, backs each cluster's
tenant secrets with HashiCorp Vault, and keeps everything converged with a level-triggered
reconciliation loop backed by Postgres - as Podman containers (default: inside WSL2) or a Helm release.

It is no longer "just a demo": it is a **functional, lab-scale platform** used for real at small scale.
The load-bearing control-plane patterns are built for real; a few peripheral concerns are deliberately
lab-grade and marked in code with a "production would…" note (see *Fidelity stance*). The system
**runs end to end**, in both fake mode (no KVM/DB) and real mode (real VMs, real clusters, Postgres +
durable job queue).

## Read these first

The [`docs/`](docs/README.md) directory documents the current system, grounded in the code. The docs
are audience-split and describe the platform **as-is** in present tense - they deliberately do **not**
repeat the load-bearing rationale this file carries. Start with:

- [`docs/concepts/architecture.md`](docs/concepts/architecture.md) - the control-plane design: the
  reconciliation loop, the state machine, the components, and the fake/real seams.
- [`docs/concepts/keeping-clusters-healthy.md`](docs/concepts/keeping-clusters-healthy.md) - the
  self-healing story: health, repair, backups, cert rotation, etcd maintenance.
- [`docs/deploy/`](docs/deploy/README.md) - operator guide: Compose/Helm deployment, providers
  (libvirt/vSphere/Proxmox), integrations (LDAP/DNS/Vault/NetBox), golden images, the
  [release workflow](docs/deploy/releasing.md), and the full
  [configuration reference](docs/deploy/configuration.md).
- [`docs/portal/`](docs/portal/README.md) - the end-user guide to every portal page (image-heavy).
- [`docs/concepts/why-kubeharbor.md`](docs/concepts/why-kubeharbor.md) - why the project exists.

This file (`CLAUDE.md`) remains the detailed **design record and rationale** - the "load-bearing" and
"production would…" notes live here and beside the code, not in `docs/`. Prefer reading the code to
infer behaviour; keep both the code comments and `docs/` in sync when you change it.

Note on identity: the product is branded **KubeHarbor** ("Kubernetes Without the Rough Seas"); the
codebase, module path, and env prefix stay `kaas` / `KAAS_`.

## The one thing to internalize

This is **a control plane driven by a level-triggered reconciliation loop**, not a "web form
that runs a script." Desired state lives in Postgres; a reconciler continuously diffs desired vs.
observed state and advances each cluster one idempotent step at a time; the API only writes
desired state. Every reconcile step **must be idempotent** - it is retried on failure and re-run
on every tick. This is what makes the system self-healing.

Self-healing extends past provisioning: a Ready cluster whose nodes go bad is **detected, escalated
and repaired** without anyone asking, and every cluster is **backed up on a cadence** so even a dead
sole control plane comes back. See *Automatic cluster and node repair* and *Periodic control-plane
backups*. The interesting half of both is the set of conditions under which the platform **refuses to
act** - a repairer without those is a cluster shredder.

## Stack

- **Backend:** Go (`cmd/api`, `cmd/worker`; logic under `internal/`).
- **State + queue:** Postgres - desired/observed state *and* the durable job queue
  (`riverqueue/river`). Unset `DATABASE_URL` falls back to an in-memory store + tick loop.
- **VM infra:** OpenTofu, one workspace per cluster - the `dmacvicar/libvirt` provider for KVM
  (module `infra/libvirt/`, wrapper `internal/provision/tofu`; the module is written against the
  **0.9** schema, a ground-up rewrite in which HCL maps ~1:1 onto libvirt's own XML - and where the
  provider refuses every volume update but forgets to mark the creation-time inputs as forcing
  replacement, so the module supplies that itself via `terraform_data.node_volume_shape`, which is
  what makes a rolling OS upgrade rebuild a node rather than fail its reconcile step forever),
  `hashicorp/vsphere` for vSphere
  (module `infra/vsphere/`, wrapper `internal/provision/vsphere`) and `bpg/proxmox` for Proxmox VE
  (module `infra/proxmox/`, wrapper `internal/provision/proxmox`); shared mechanics in
  `internal/provision/tofurunner`.
- **Config mgmt:** Ansible with a dynamic inventory generated per run from the DB; the wrapper
  shells out to `ansible-playbook`.
- **VM images:** golden images built with Packer (`kubeadm`/containerd pre-baked; Ansible only
  *forms* the cluster).
- **Add-ons:** Helm, driven from the catalog-as-data.
- **Secret store:** a central HashiCorp Vault (`internal/vault`, real impl `internal/vault/hcvault`)
  gives each cluster its own KV subtree and policies mirroring the portal's read/write model; the
  bundled **external-secrets** add-on consumes it in-cluster, and the portal's Secrets page surfaces it.
  See *Secret store*.
- **Catalog:** `internal/catalog/catalog.json` - OS images, Kubernetes versions, add-ons, and
  release *bundles* with `supersedes` upgrade chains. Editing versions is a data change. The default
  bundle ships a batteries-included set: Cilium (CNI), Longhorn (storage), MetalLB + Envoy Gateway
  (ingress), cert-manager (TLS), external-dns, external-secrets, kube-prometheus-stack, metrics-server,
  and trivy-operator. That set is **locked on at create time** and removable only from a Ready cluster;
  `KAAS_BUNDLE_ADDONS_OPTIONAL` lifts the lock, because on a laptop-scale KVM host the bundle can
  outweigh a small cluster's own workers and installing all of it is what tips the cluster over. Two
  things are load-bearing: the lock lives in **`app.resolveAddons`, not the portal** (the wizard renders
  the padlocks, the API is the gate - same split as every other tenancy decision), and a create
  request's `addons` distinguishes **nil from empty** - omitted means "the bundle's set", present-but-
  empty means "none at all", which is the only way "I want no add-ons" is expressible. Nothing else
  changes: every wiring step already gates on its own add-on being installed
  (`reconcileGatewayWiring`, `reconcileMonitoringWiring`, `reconcileVaultWiring`, `reconcileDNSWiring`,
  and the per-worker Longhorn disk), so this lifts an admission-time restriction and adds no new
  tolerance.
- **Custom catalogs:** per-user, self-defined Helm-chart add-ons (`internal/domain.CustomCatalog`,
  `internal/app/customcatalog.go`) - the tenant-facing counterpart to the built-in catalog, owned and
  group-shared like clusters. Selecting one onto a cluster copies its chart definition into the
  per-cluster add-on, so the reconcile loop stays untenanted. See `docs/concepts/architecture.md`.
- **Web portal:** a React + TypeScript + Vite + **Mantine** SPA (`web/portal/`), served by its
  own nginx container that reverse-proxies `/api/*` to the API (`deploy/Containerfile.web`,
  `deploy/nginx.conf`). It is the browser front door on `:8080`; the API is a pure JSON+SSE
  surface published on `:8081`. `make up`/`up-fake` build and run it.
- **Runtime:** Podman containers, all inside WSL2.

## Seams (fake vs. real)

The control loop depends only on interfaces, each with a fake and a real implementation selected
by env var in `internal/app`. Keep this seam intact - new backends plug in here, and the fakes
keep the system runnable and testable without KVM or a database.

| Seam | Selector | Fake | Real |
|---|---|---|---|
| `store.Store` | `DATABASE_URL` | in-memory | Postgres + River |
| `authn.Authenticator` | `KAAS_LDAP` (+ `KAAS_AUTH`) | in-memory directory | Active Directory / LDAP |
| `provision.Provisioner` | `KAAS_PROVISIONER` (+ `KAAS_INFRA_PROVIDERS`) | pretend IPs | OpenTofu/libvirt (kvm), OpenTofu/vSphere, OpenTofu/Proxmox |
| `config.Manager` | `KAAS_CONFIG` | plausible kubeconfig | Ansible/`kubeadm` |
| `addons.Manager` | `KAAS_ADDONS` | instant success | Helm |
| `metrics.Collector` | `KAAS_METRICS` | synthetic load | `kubectl` → metrics.k8s.io |
| `health.Checker` | `KAAS_HEALTH` | synthetic all-healthy | `kubectl` → API-server checks |
| `shell.Backend` | `KAAS_SHELL` | in-process simulated kubectl | worker-proxied bash+kubectl PTY |
| `nodessh.Backend` | `KAAS_NODE_SSH` | in-process simulated Linux shell | node-ssh-sandbox-proxied `ssh` PTY |
| `kube.Client` | `KAAS_KUBE` | synthesized workloads + storage | worker-proxied `kubectl` (JSON/logs) |
| `monitoring.Querier` | `KAAS_MONITORING` | synthesized telemetry | worker-proxied PromQL (`kubectl get --raw`) |
| `security.Querier` | `KAAS_SECURITY` | synthesized Trivy reports | worker-proxied Trivy CRD reads (`kubectl get`) |
| `audit.Querier` | `KAAS_AUDIT` | synthesized audit events | worker-proxied apiserver-log reads (`kubectl logs`) |
| `tunnel.Proxier` | `KAAS_TUNNEL` | synthesized landing page | worker-proxied HTTP to in-cluster UIs (`services/proxy`) |
| `dns.Registrar` | `KAAS_DNS` | logs what it would publish | `nsupdate` RFC 2136 dynamic update (GSS-TSIG) |
| `vault.Manager` | `KAAS_VAULT` | in-memory (logs) | HashiCorp Vault (`internal/vault/hcvault`) |

**Directory authentication** (`internal/authn`) is the one seam whose axis is not fake/real but
*mechanism*: `KAAS_AUTH=local|ldap` decides whether the portal authenticates against Active
Directory at all, and `KAAS_LDAP=fake|real` is the ordinary fake/real switch underneath it - the
same orthogonality as `KAAS_INFRA_PROVIDERS` vs `KAAS_PROVISIONER`, so `KAAS_AUTH=ldap
KAAS_LDAP=fake` demos the whole AD flow with no domain controller. Mapping rules live in a **mounted
YAML file** (`KAAS_LDAP_CONFIG`, see `deploy/ldap.example.yaml`) rather than env, because they are a
list of raw LDAP filters. Each rule's filter is evaluated as a **BASE-scoped search against the
authenticating user's own DN** - which turns any filter the directory can express into a group
predicate (`memberOf`, AD's nested-group matching OID, any attribute at all) with no `memberOf`
parsing of our own. Several rules may share a `group_key` and so land in one group with different
roles (the "K8s-Eng reads, K8s-Eng-Admins writes" shape); a user matching several gets the highest.
Accounts are provisioned on first login at **zero quota**, and their directory-driven memberships are
recomputed on every login. Four things are load-bearing and easy to break:

- **Rules grant group roles only, never platform admin.** `domain.User.IsAdmin` is seed-only, and
  `quota.Allocated` excludes admins from the conserved pool - so a directory-driven admin toggle
  would silently move capacity in and out of the platform's allocation total.
- **The seeded local admin is break-glass** and is tried before the directory: `make kubeconfig` and
  `deploy/teardown-clusters.sh` authenticate as it, and a DC outage must not lock the platform out.
  Its username is owned exclusively - the directory can never claim it.
- **Every writer of a user row shares `LockAdmission`** (`internal/app`, the `…Locked` split). The
  users table is the quota ledger and `Store.UpdateUser` rewrites the whole row, so an admin's quota
  grant and a concurrent login would clobber each other. Locks **do not nest** - the Postgres one
  takes a fresh connection per call, so a nested acquire hangs forever - and the LDAP dial stays
  strictly outside them.
- **Memberships merge by group ownership** rather than replace (`mergeMemberships`): the Admin page
  sends a user's whole membership list on every edit, so rejecting one that names a directory group
  would make adding a directory user to a *local* group impossible.

`POST /auth/login` is public and unauthenticated, so it is **throttled in Postgres** (per-username
and per-IP; the per-username counter is the one that holds when an attacker rotates addresses).
Without it, every failed login is a real bad-password bind against a real AD account and anyone who
can reach the portal could lock out the domain. Only the **API** ever gets any of this - the worker
does not authenticate users, so the bind password stays out of the container holding the libvirt
socket and every tenant's secrets.

The **cluster shell** (`internal/shell`; the portal's Terminal tab, `GET /clusters/{id}/shell` over
WebSocket) is request-driven, not part of the reconcile loop. The API can't reach cluster VMs, so
`KAAS_SHELL=worker` proxies each session to a host-networked exec agent (`KAAS_SHELL_LISTEN`) that
runs a real `bash`+`kubectl` PTY; the fake is an in-process pseudo-terminal that synthesizes kubectl
output from cluster state. For isolation the real agent runs in a **dedicated, unprivileged `shell`
sandbox** (`cmd/shell-agent`, `deploy/Containerfile.shell`, the `shell` compose service) - bash +
kubectl only, non-root, all-caps-dropped, read-only rootfs, and holding **none** of the worker's
secrets, keys, libvirt socket or DB - so a user's terminal never lands on the privileged worker;
the PTY also hands the shell a scrubbed, allowlisted environment so no process-env secret leaks via
`env`. Same exec agent still serves the `/kube-exec`/`/kube-logs` seams below. (Only in the
single-process dev path, `make run-worker`, does the worker itself host the agent.)

**Node SSH** (`internal/nodessh`; the portal's Nodes tab → **SSH** button, `GET
/clusters/{id}/nodes/{vm}/ssh` over WebSocket) gives a browser terminal *inside a cluster VM* - an
`ssh` session as the `kaas` user (the passwordless-sudo account cloud-init injects) - for OS-level
inspection the kubectl shell can't do. It is **write-scoped** (`authorizeClusterWrite`; a read-role
group-mate gets 403), which is not an escalation: a write-role member already holds the admin
kubeconfig, and a privileged pod is root on these nodes anyway. Unlike every other cluster surface it
does **not** gate on phase Ready - it needs only a booted VM with an IP (`node.IP != ""`), because a
half-provisioned cluster is exactly when getting onto the box to read journald is useful. The API
authors the target IP from the cluster's own node row; the browser names only a VM. Sessions are
audited (open/close `events.Event`, `Source: "ssh"`) on the Activity tab, and idle out after 30m.

The load-bearing decision is **where the SSH key lives**. The platform's cluster-VM key
(`KAAS_SSH_PRIVATE_KEY_FILE`) is a single fleet-wide credential and the whole point of the `shell`
sandbox is that it holds *no* secrets - so node SSH cannot reuse it (a bash user would `cat /keys/id`
the fleet key) nor go on the worker. It gets its **own** sandbox with its own binary
(`cmd/node-ssh-agent`, `deploy/Containerfile.nodessh`, the `nodessh` compose service, `KAAS_NODE_SSH=agent`
→ port `:8084`, its own token `KAAS_NODE_SSH_TOKEN`): host-networked, all-caps-dropped,
read-only rootfs, holding the VM key (and, for a remote hypervisor, the bastion key) but **no** DB,
libvirt socket or `KAAS_SECRET_KEY`. Its containment is the **inverse** of the shell sandbox's - it
*does* hold a credential, and is made safe by holding **no shell**: the only process it ever starts is
`ssh` to the caller-named IP (`internal/nodessh/sshpty`), never bash, never `sh -c`, so there is no
session from which to read the key. A separate binary specifically so the key-holding image is
*incapable* of serving a PTY. Unlike the shell sandbox it runs as **root** (like the worker, which
holds the same key): ssh refuses a group/other-readable private key, so the `0600` key is readable
only by its owner - so containment here rests on the no-shell property and the caps/rootfs hardening,
not on the uid. *Production would* fork the `ssh` child under an unprivileged uid from a privileged
supervisor that reads the key. The `ssh` argv is fully server-authored and four flags are load-bearing
- `-e none` (no `~` escape command line), `-F /dev/null` (no ssh_config injection),
`IdentitiesOnly=yes`, `BatchMode=yes` - and the agent re-runs `net.ParseIP` on the handshake IP so a
smuggled `-oProxyCommand=…` can never reach the command line. For a remote KVM host it chains the
`kvmhost.Host.ProxyCommand()` bastion hop exactly as Ansible does. *Production would* run an SSH CA
minting a short-lived, single-node certificate per session instead of mounting a fleet-wide key.
The fake (`KAAS_NODE_SSH=fake`) synthesizes a plausible Linux shell (via the shared `shell.Emulator`)
so `up-fake` demos the button. Same request-driven, horizontally-scaled shape as the shell agent
(`KAAS_NODE_SSH_AGENT_ADDR` is a comma-separated pool; `internal/execagent` round-robins + fails over).

The **Workloads page** (`internal/kube`; the portal's Workloads nav item, `GET /clusters/{id}/workloads…`)
is likewise request-driven, not part of the reconcile loop: it lists and inspects the live workloads
inside a Ready cluster (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs) with their pods, YAML,
events and **live-streaming logs**, and scales them. Same networking constraint and transport as the
shell - `KAAS_KUBE=worker` forwards each `kubectl` invocation to the same worker exec agent
(`/kube-exec` for one-shot JSON, `/kube-logs` for `kubectl logs -f`); the fake synthesizes a plausible
workload set from control-plane state. Reads are view-scoped; **scale** is write-scoped (a read-role
group member gets a 403, and every call forwards that actor's own **per-user** kubeconfig - a reader
gets the RBAC-limited reader cert, so the mutation is both blocked and attributed; see
`app.userClusterKubeconfig`).

The **Storage page** (`internal/kube/storage.go`; the portal's Storage nav item, `GET
/clusters/{id}/storage/…`) shares that same seam rather than getting one of its own, because
PersistentVolumeClaims and StorageClasses are **core Kubernetes objects**: same `kubectl get -o json`,
same cluster, same transport. It is also where the platform's default storage becomes visible - the
`longhorn` StorageClass every cluster ships with, and an "Open UI" link to Longhorn's own console
through the tunnel seam (see *Default storage*). So it needs **no add-on** (unlike Monitoring/Security) and gates only on
Ready. Two tabs, both with the cluster/namespace pickers: **Claims** (per-PVC Overview - binding
state, capacity granted vs. requested, the bound PersistentVolume, the pods mounting it - plus Events
and YAML) and **StorageClasses** (details + YAML; cluster-scoped, so the namespace picker hides). It is
entirely **read-only** and view-scoped, and notably takes **no** admin-kubeconfig shortcut: a read-role
member's own per-user reader credential already covers all of it - PVCs/pods/events via the built-in
`view` role, PVs and StorageClasses via the small cluster-scoped read role the `kaas:readers` group is
bound to (`ansible/roles/viewer_kubeconfig`).

The **Networking page** (`internal/kube/network.go`; the portal's Networking nav item, `GET
/clusters/{id}/networking/…`) shares that same seam for the same reason: Services are **core Kubernetes
objects** and the Gateway API's Gateways/Routes are **ordinary CRDs** - same `kubectl get -o json`, same
cluster, same transport, so no seam of its own and no admin shortcut. It is where the platform's
north-south contract (*Default gateway* and *Cluster DNS*, below) finally becomes visible: an
**Overview** showing the reserved `LoadBalancerIP` and whether `GatewayWired` applied it, the wildcard
record and whether `DNSWired` published it, the default Gateway's listeners (is HTTPS on?), and
**exposed applications** - every hostname reachable from outside, with its URL, the route publishing it,
its backends, and whether the platform's wildcard covers the name or the user owns its DNS - plus
**Services**, **Gateways** and **Routes** tabs over the raw objects. Three things are load-bearing:

- **"Exposed" is derived server-side** (`kube.ExposedApps`), above the fake/real seam, so both backends
  mean the same thing by it: routes joined to the Gateways that accepted them, a hostname-less route
  inheriting its listener's hostnames, and http-vs-https decided by the Gateway API's own hostname
  intersection against the terminating listeners.
- **A missing Gateway API is empty, not an error.** `envoy-gateway` is deselectable and `kubectl get` on
  an unknown type is a hard failure, so every Gateway API read goes through `optional()`. The page has
  deliberately **no add-on gate** (unlike Monitoring/Security) - the reserved address is still the
  cluster's, and the Services tab still works.
- **The `view` role does not cover CRDs.** Read-only and view-scoped throughout on the actor's own
  per-user credential, which meant granting `gateway.networking.k8s.io` in the `viewer_kubeconfig`
  role's cluster-read ClusterRole - otherwise a reader cannot list the very Gateway they are told to
  attach their routes to.

The **Monitoring page** (`internal/monitoring`; the portal's Monitoring nav item,
`GET /clusters/{id}/monitoring…`) is the third request-driven query seam: it runs curated PromQL
against a Ready cluster's in-cluster Prometheus (installed by the **kube-prometheus-stack** add-on)
and renders native panels, organized into titled sections per tab with per-panel info tooltips - an
API-server availability SLO + capacity commitments (Overview), control-plane component health
(apiserver/etcd/controller-manager/scheduler/kube-proxy/coredns/kubelet), USE resource saturation
(utilisation + saturation pairs per CPU/memory/disk/network), per-namespace/pod workload usage
(Workloads), Cilium networking, and firing alerts - leaning on the stack's own recording & alerting
rules. Panel kinds span gauges, sparkline stat tiles, line/area/stacked time-series, and top-k bar
lists; the registry (`monitoring.Tabs`) is data, so adding a panel is a spec entry, not new code. Same
networking constraint and transport as the shell/Workloads: `KAAS_MONITORING=worker` forwards each
query as a `kubectl get --raw .../services/proxy/api/v1/query` to the same worker exec agent
(`/kube-exec`); the fake synthesizes plausible, drifting telemetry. View-scoped, and the query runs
with the cluster **admin** kubeconfig server-side (read-only aggregate metrics, no secret exposure -
the `view` role can't `get services/proxy`; production would mint a Prometheus-scoped read token).
The add-on is installed **first** among a cluster's add-ons so its Prometheus Operator + ServiceMonitor
CRD exist before `metrics-server` and Cilium (re-)publish their ServiceMonitors (the latter via
`config.Manager.EnsureCNIMetrics`, since the CNI is installed at bootstrap before any add-on).
kubeadm's defaults are not monitoring-friendly out of the box - kube-scheduler/kube-controller-manager
bind their metrics port to loopback, kube-proxy's metrics server does too, and etcd exposes no
unauthenticated metrics endpoint at all - so kube-prometheus-stack's built-in ServiceMonitors for
those four components (all enabled by default) scrape nothing until `config.Manager.
EnsureControlPlaneMetrics` fixes it (kubeadm manifest edits + a kube-proxy ConfigMap patch; no CRD
dependency, but gated on the stack being installed like `EnsureCNIMetrics`). Both run from
`reconcileMonitoringWiring`, right after add-ons install.

The **Security page** (`internal/security`; the portal's Security nav item, `GET /clusters/{id}/security…`)
is the fourth request-driven query seam: it reads the **Trivy Operator's** report CRDs from a Ready
cluster (installed by the **trivy-operator** add-on) and renders the cluster's security posture - a
cluster-wide Overview (per-kind severity rollups, the most vulnerable images, per-namespace risk) plus
a searchable, severity-filterable, drill-into-findings table per report family: **VulnerabilityReport**
(image CVEs), **ConfigAuditReport** (workload misconfigurations), **ExposedSecretReport** (secrets baked
into images), and **RbacAssessmentReport** (over-permissive Roles/ClusterRoles). Trivy Operator does the
scanning *inside* the cluster and writes findings back as CRs; this seam only reads them. Same networking
constraint and transport as the shell/Workloads/Monitoring: `KAAS_SECURITY=worker` forwards each read as a
`kubectl get <report>.aquasecurity.github.io -A -o json` to the same worker exec agent (`/kube-exec`); the
fake synthesizes a plausible, deterministic set of reports from control-plane state. View-scoped, and the
query runs with the cluster **admin** kubeconfig server-side (read-only security-posture metadata - Trivy
redacts matched secret values; the `view` role doesn't cover Trivy's CRD group, so production would mint a
Trivy-scoped read token). Unlike kube-prometheus-stack the add-on needs **no** post-install wiring - the
Helm chart ships the CRDs and the operator, so it "just works" once installed (default priority, so it
lands after the monitoring stack, letting its own ServiceMonitor be scraped).

The **Audit tab** (`internal/audit`; the portal's cluster-detail **Audit** tab, `GET
/clusters/{id}/audit`) is the fifth request-driven query seam: it renders the cluster's Kubernetes
API-server audit trail - a filterable "who changed what" feed (verb, actor, resource, response,
timestamp) with per-event detail. **Audit logging is on by default in every cluster**, enabled not by
an add-on but by the **`controlplane_audit` ansible role**, which drops a curated audit policy and a
`kubeadm --patches` JSON6902 patch that adds the audit flags + policy volume to the kube-apiserver
static pod (`ansible/roles/controlplane_audit`, applied on both `kubeadm init` and control-plane
`join`). The **backend is the apiserver's own stdout** (`--audit-log-path=-`), so reading events back
is just `kubectl logs kube-apiserver-<node>` - the **same worker exec agent** (`/kube-exec`) the
Workloads/Monitoring/Security seams use, no new component and no new API↔worker hop: `KAAS_AUDIT=worker`
lists the apiserver pods (one per control plane, merged for HA) and tails each, parsing the JSON audit
`Event` lines out of the interleaved klog output. The fake synthesizes a plausible, live-drifting
stream from control-plane state (a new event arrives at the top every ~12s) so the tab is demoable
under `up-fake`. A tail is megabytes of interleaved klog+audit JSON per control plane, so the fetched
window is **cached per cluster for a few seconds** (`audit/kubectl/cache.go`) and the control planes
are tailed in parallel: the cached window is **unfiltered** and `audit.Assemble` applies the query
above it, so a filter change, a keystroke or a second viewer costs nothing and only the portal's poll
pays for `kubectl`. The cache is keyed on cluster ID alone (an audit window is cluster-wide observed
state read with the admin kubeconfig for everyone; tenancy is enforced before the querier) and is
per-replica in-process, which is fine - a miss just re-reads. A read where **every** apiserver failed
is an error, not an empty page, so it is never cached; a stale window is served over a transient
failure rather than blanking the feed. Gates only on **Ready** (no add-on gate - a pre-audit cluster just returns an empty
feed). View-scoped, and the query runs with the cluster **admin** kubeconfig server-side (reading the
apiserver pod's log isn't something the `view` role grants). Two deliberate shortcuts in the repo's
style: the apiserver logs audit to its own stdout rather than a real sink (**production** ships to
Loki/ELK/a webhook), and the read uses the admin kubeconfig rather than a scoped token. The curated
policy keeps the trail focused and stdout volume bounded - it logs mutations at Metadata (RBAC writes
at RequestResponse) and drops pure reads and the high-volume lease/event/health noise.

The **in-cluster UI tunnel** (`internal/tunnel`; the Monitoring and Storage pages' "Open UI" links,
`ANY /clusters/{id}/proxy/{app}/…`) is the sixth request-driven seam and the only one that is a full
streaming HTTP reverse proxy rather than a query. kube-prometheus-stack deploys Grafana, Prometheus and
Alertmanager with their own web UIs, but the platform installs no ingress/gateway controller, so those
UIs have no external address; the tunnel gives the browser a same-origin route to them, reverse-proxied
per cluster through the API server's `services/<svc>:<port>/proxy` - API → the same worker exec agent's
new `/http-proxy` endpoint → the service proxy (two chained `httputil.ReverseProxy`, one per network
hop). The one hard problem - these apps emit **absolute-path** asset URLs - is solved not by rewriting
responses (fragile) but by configuring each app's route-prefix **at install** to the tunnel path
(`tunnel.RoutePrefix` = `/api/clusters/<id>/proxy/<app>`, keyed on the cluster ID, known at add-on
install): Grafana `serve_from_sub_path`+`root_url`, Prometheus/Alertmanager `routePrefix`+`externalUrl`,
templated from the catalog values by the helm manager's `{{.ClusterID}}` substitution. Because
Prometheus's route-prefix relocates **every** Prometheus route, the Monitoring page's PromQL querier
(`internal/monitoring/promql`) now prepends the same prefix to its `services/proxy` path. It proxies
with the cluster **admin** kubeconfig server-side (the `view` role can't `get services/proxy`), so
production would mint a per-app scoped token - the browser never sees the kubeconfig (it rides the
internal API→agent hop only). The fake (`KAAS_TUNNEL=fake`) serves a synthesized landing page so
`up-fake` demos the links.

**Tenancy is per-app**, because these UIs know nothing about the platform's read/write role and each
answers to a different auth model:

- **Grafana** has a real user model, so the role rides in as an **auth-proxy identity**: the Proxier
  sets `X-Webauth-User`/`X-Webauth-Role` from the server-resolved actor (`accessTo`: view → `Viewer`,
  full → `Editor` - not Admin; "write on the cluster" is editing dashboards, not administering
  Grafana), and the add-on enables `[auth.proxy]` at install. No login screen, no shared password.
  **These headers are trusted by Grafana, so the Proxier must delete any client-supplied copy before
  setting them** - otherwise a user forges their own role (same class as the `Origin` strip below).
  It is safe only because the tunnel is the sole network route in; production would also pin
  `[auth.proxy] whitelist`.
- **Prometheus** is read-only in practice (the operator leaves `enableAdminAPI` false) → view-scoped.
- **Alertmanager** ships **no auth** and its UI silences alerts, and it has no user model to express
  our split - so the whole app is gated on write (`tunnel.App.WriteScoped` → `authorizeClusterWrite`,
  403 for a read-role group-mate). The portal hides the link; the API is the authoritative gate.

Two headers are stripped on the way to the app: `Accept-Encoding` (so Go's transport negotiates gzip
itself and transparently decompresses, keeping the body rewritable) and `Origin` - the app's CSRF
check compares it against the Host it sees, which behind the tunnel is the API *server's* and can
never match, so Grafana would 403 `origin not allowed` on every login. The tunnel's real CSRF
boundary is the API's `SameSite=Lax` session cookie. The agent also **undoes the API server's own
response rewriting**: `services/proxy` prepends its internal path to absolute URLs (`<base href>`,
`Location`), which the browser can't reach, so the agent strips that deterministic prefix back out.
WebSocket-only features (Grafana Live) may not upgrade through the service proxy - the core UIs are
plain HTTP and unaffected.

## Default gateway (MetalLB + Envoy Gateway)

Every cluster ships north-south ingress by default: the **`metallb`** and **`envoy-gateway`** add-ons
are in the bundle, and the platform reserves **one node-network address** (`domain.Cluster.
LoadBalancerIP`) for the default MetalLB **L2 (ARP) pool**, from which the default Envoy Gateway draws
its external IP. Like `APIVIP`, the address is desired state decided **once at admission** under
`store.LockAdmission` - derived deterministically on kvm (`netpool.LoadBalancerIP`, a fixed high host
one slot below the VIP), allocated from the operator range on a shared-network **static** cluster, and
**user-supplied** on a shared-network **dhcp** cluster (required on *every* dhcp cluster, not only HA
ones - the platform can't know a free host outside the DHCP pool, the same reason `api_vip` is
user-supplied there). On the shared-network providers it is registered in **NetBox** alongside the node
IPs and VIP.

Applying the CRs is a gated post-install step, mirroring `reconcileMonitoringWiring`:
`reconcile.reconcileGatewayWiring` → `config.Manager.EnsureDefaultGateway` (the **`default_gateway`**
ansible role, distinct from the HA `loadbalancer` role) runs once the add-ons are installed and the
address is set, and applies (idempotent `kubectl apply` on `control_plane[0]`) a MetalLB `IPAddressPool`
(a single `/32`) + `L2Advertisement`, and the Envoy `GatewayClass` (the cluster's default Gateway API)
+ a default `Gateway` bound to it and pinned to the reserved IP. Five things are load-bearing:

- **The reserved IP is per-cluster, not per-node**, so it stays reserved (never recomputed) across pool
  edits - `admitSharedNetwork`/`scaleSharedStaticIPs` keep it in the `used` set alongside the VIP.
- **The `GatewayWired` marker gates re-runs**, like `MonitoringWired` - but nothing clears it, because
  the CRs live in etcd and a CNI/OS upgrade or node roll doesn't undo them.
- **The wiring gates on both add-ons being installed**, so a user who deselects `metallb`/`envoy-gateway`
  at create gets no pool/Gateway (the reservation is harmless).
- **The same one-shot pass also wires HTTPS** (below), so it additionally holds on `cert-manager` being
  installed *when that add-on is selected* - otherwise `GatewayWired` would latch with the TLS half
  never applied (`gatewayWiringReady`).
- **A single-IP pool is a deliberate demo shortcut** - production would carve a real pooled range and
  mint per-app scoped tokens rather than lean on the admin path.

**HTTPS by default (cert-manager, self-signed).** The **`cert-manager`** add-on is in the bundle too,
and when it's on the cluster the same `default_gateway` role makes north-south routes TLS-ready with
nothing to configure: it applies a self-signed **`kaas-selfsigned` ClusterIssuer** and - *when the
cluster owns an apps domain* (`Cluster.AppsDomain`, set at admission, see *Cluster DNS*) - a wildcard
**`Certificate`** for `*.<apps domain>` into the Gateway's own namespace, plus a second **HTTPS :443
listener** on the default Gateway that terminates it (`tls.mode: Terminate`, `certificateRefs` → the
issued `kaas-default-tls` Secret, `hostname: *.<apps domain>`). So any `HTTPRoute` a user attaches for
a name under the apps domain is reachable over HTTPS the moment the cluster is Ready. Load-bearing:

- **The apps domain drives the cert**, and it's known at gateway-wiring time because it's derived at
  admission - even though `reconcileDNSWiring` (which *publishes* the wildcard record) runs after. No
  apps domain ⇒ only the ClusterIssuer is created (a user can annotate their own routes); no platform
  HTTPS listener, since there's no hostname to secure.
- **cert-manager issues asynchronously**; the listener tolerates the not-yet-present Secret and programs
  itself once it appears, so the single idempotent `kubectl apply` converges with nothing to wait on.
- **Self-signed is a deliberate demo shortcut** (each issued cert is its own root - browsers won't
  trust it) - production would run a trusted CA or ACME (Let's Encrypt) issuer instead.

## Cluster DNS (delegated zone + external-dns)

When `KAAS_DNS_BASE_DOMAIN` is set, every cluster owns a subdomain of a **delegated zone**
(`<cluster>.kaas.example.internal`) and the platform publishes **one** record in it -
`*.apps.<cluster>.kaas.example.internal A <Cluster.LoadBalancerIP>`, the address the default Envoy
Gateway holds. That is the whole user contract: attach an `HTTPRoute` for any name under it to the
default Gateway and it resolves and routes, with nothing to configure. Both names (`Cluster.DNSDomain`,
`Cluster.AppsDomain`) are derived **once at admission** from `dns.Settings` and stored on the row, like
`APIVIP`/`LoadBalancerIP`; no allocator is needed because cluster names are already globally unique.
Publishing is `reconcile.reconcileDNSWiring` → `dns.Registrar.EnsureCluster`, gated on `GatewayWired`
and marked by `DNSWired`; the real registrar (`internal/dns/nsupdate`) shells out to `kinit` +
`nsupdate -g` (delete-then-add = idempotent upsert) against the AD DCs' secure-update zone.

**Windows DNS Server refuses to create the wildcard over RFC 2136 at all.** Confirmed against a real
DC: a plain (non-wildcard) name updates fine via `nsupdate` against the zone; the identical request
for a `*.foo` owner name comes back `NOTAUTH`/`REFUSED` regardless of the zone's dynamic-updates
setting or `KAAS_DNS_AUTH` - Microsoft's dynamic-update handler rejects wildcard RRs unconditionally.
Since the wildcard is the *only* record this registrar ever writes, `nsupdate` can never work for it
against a Windows DC. `KAAS_DNS=winrm` (`internal/dns/winrm`) is the escape hatch: it bypasses RFC
2136 entirely and drives the DNS Server role's own PowerShell module
(`Add-DnsServerResourceRecordA`/`Remove-DnsServerResourceRecord`/`Get-DnsServerResourceRecord`) over
WinRM, which has no such restriction - same Get-then-conditional-Remove-then-Add idempotent-upsert
shape as `nsupdate`'s delete-then-add, same `dns.Registrar` interface, so `reconcileDNSWiring`/
`releaseDNS` don't know the difference. It reuses `KAAS_DNS_SERVER`/`_ZONE`/`_TTL` (as the
`-ComputerName` target and `-ZoneName`) - only the WinRM transport and credential are new
(`KAAS_WINRM_HOST`/`_USERNAME`/`_PASSWORD`). external-dns is untouched by any of this: it only ever
creates concrete, non-wildcard hostnames for a user's Services/Ingresses/HTTPRoutes, so it keeps
using `nsupdate`/GSS-TSIG regardless of which registrar publishes the platform's own record. Shortcut,
in the repo's style: WinRM auth defaults to NTLM (works against a default `winrm quickconfig`
listener) rather than Kerberos - production would use Kerberos there too, the same shape as
`nsupdate`'s GSS-TSIG.

Four things are load-bearing:

- **The wildcard is PLATFORM-owned, not external-dns's.** external-dns lives in the cluster, so
  nothing inside it can delete its records when the cluster is destroyed - and `LoadBalancerIP` is
  recycled to the next cluster on that subnet, so an orphan would resolve into **another tenant's**
  gateway. Lifecycle = cluster lifecycle = the control plane's job.
- **Release runs BEFORE `DestroyCluster`** (`releaseDNS` in `PhaseDeleting`), for that same reason.
  Reversing the order is the bug the ordering exists to prevent (`TestDNSReleasedBeforeDestroy`).
  A DNS failure **fails the reconcile step** and retries, exactly like NetBox registration.
- **external-dns is bundled and owns the OTHER half** - the names a user's Services/Ingresses/
  HTTPRoutes ask for. It is configured per cluster through `addons.Extras` (`internal/app/dns.go`),
  a hook for deployment-shaped values the catalog cannot carry, applied as `--set` **last** so a
  user's values override can't unhook their cluster from the platform's DNS. Its `domainFilters` is
  the cluster's own subdomain, its `txtOwnerId` the cluster id - and it leaves the platform's
  wildcard alone because that carries no ownership TXT. The `gateway-httproute` source is added only
  when `envoy-gateway` is on the cluster (the source fails its initial sync without the CRDs).
- **The credential is worker-only and never a Helm value.** `KAAS_DNS`+credentials go to the worker
  (the API only names domains at admission - hence `Settings.Validate` vs `ValidateUpdate`), and the
  in-cluster copy is written as a **Secret** the chart references by `secretKeyRef`; a Helm value
  would land in the Deployment's env, readable by any cluster user who can read a pod spec.

*Production would* delegate a zone per cluster with a credential scoped to it (today one service
account can write the whole zone, so the per-cluster domain filter is a guardrail, not a boundary),
and sweep a deleted cluster's leftover external-dns records under the leader lease (release withdraws
the platform's wildcard only - enumerating the rest needs a zone transfer we don't ask for).

## Secret store (HashiCorp Vault + external-secrets)

The platform's tenant-facing secret store is a **single, central HashiCorp Vault** (`internal/vault`,
seam `vault.Manager`, real impl `internal/vault/hcvault`, selected by `KAAS_VAULT=fake|real`). There is
**one** Vault, deployed next to the platform (a compose service; a dependency of the helm chart);
"per-cluster" means a KV *path* and a set of policies/roles, **never a Vault per cluster**. Under the
KV v2 mount (default `kaas`, `KAAS_VAULT_MOUNT`):

```
kaas/platform/*                       platform-owned secrets (admins only)
kaas/clusters/<cluster-id>/<ns>/<n>   per-cluster, per-namespace tenant secrets
```

The bundled **`external-secrets`** add-on consumes a cluster's subtree from *inside* the cluster - a
per-cluster JWT auth role bound to the read policy, plus a `ClusterSecretStore` the wiring applies (the
`external_secrets` ansible role) - and the portal's **Secrets page** (`internal/app/secretspage.go`;
the nav's Secrets item) lists the resulting Secrets/ConfigMaps with values redacted, badging the ones
ESO synced from Vault, and hands off to the Vault UI with a short-lived minted token ("View in Vault").

**Authorization mirrors the platform.** Vault doesn't know the ownership/group model, so the platform
is the **single writer** of Vault's policies, identity groups and entities and keeps them converged
with Postgres - `DesiredAccess` is the pure mapping (owner/admin/group-role → policies), `SyncAccess`
applies it. That is what makes "only users with access to a cluster can touch its Vault path, writers
edit, readers only view" true **in Vault itself**, not only in the portal. The Vault auth backend
follows `KAAS_AUTH` (local → Vault userpass, ldap → Vault ldap, configured from the same directory
settings, translated by `app.buildVaultManager`).

Two responsibilities, split the same way the loop splits per-cluster from singleton work:

- **Per-cluster lifecycle** (`EnsureCluster`/`ReleaseCluster`) is reconcile-loop work, gated by
  `Cluster.VaultWired` and running once `external-secrets` is installed (`reconcileVaultWiring`,
  `vaultWiringReady`) - exactly like `reconcileDNSWiring`/`DNSWired`. Nothing clears the marker; the
  path is dropped by `releaseVault`, which runs in `PhaseDeleting` **before** `DestroyCluster` (same
  release-before-destroy ordering, and the same reason, as `releaseDNS`). A cluster's secret **data**
  is only ever deleted by `releaseVault`, never by a wiring re-run.
- **Access convergence** (`SyncAccess`) runs under the **leader lease on a ticker** (like GC/metrics),
  because membership edits happen API-side and never bump a cluster's generation, so the reconcile
  queue would never see them.

Load-bearing:

- **The token is split by role**, like the DNS/vCenter credentials. The **worker** holds the broad
  **management token** and provisions the mount/policies/identity/paths; the **API** holds a narrow
  **minter token** used only by `MintUserToken` for the "View in Vault" handoff. Only a process given a
  token builds the real client (`internal/app/vault.go`), and the real impl is a thin net/http client
  (the same shape as `internal/netbox`).
- **The in-cluster copy is never a static Vault token in a Helm value.** ESO authenticates with the
  per-cluster JWT auth role, so no long-lived credential lands in a pod spec - the same discipline as
  the DNS Secret vs. a Helm value.
- **The address has two consumers, and they are split.** `KAAS_VAULT_ADDR` is the platform's own route
  (API and worker); `KAAS_VAULT_CLUSTER_ADDR` is what goes into each cluster's `ClusterSecretStore`,
  so on a real cluster it must be an address ESO can reach from *inside* the cluster - a node-network
  address, not `host.containers.internal`. Unset, it falls back to `KAAS_VAULT_ADDR`, which is right
  whenever both share one route. Keeping them separate matters because collapsing them puts the
  reconcile loop on the tenant-facing route: a tunnelled or otherwise fragile address then fails
  `reconcileVaultWiring` and loops **every** cluster in `InstallingAddons`, re-running all of its
  add-on installs, where the split degrades only ESO's secret syncing - the one thing that route is
  actually for.
- **The platform only ever writes under its own `kaas` mount** and manages its own policies/identity,
  so it coexists with a Vault already used for other things.

*Production would* run HA Vault with auto-unseal via a KMS, persistent storage and an audit device; the
compose `vault` service is a **dev-mode** Vault (auto-unsealed, a fixed root token, in-memory storage)
- a deliberate lab shortcut. The Fake records state in memory and logs, so admission, wiring, the
Secrets page, and the handoff are all demoable under `make up-fake` with no Vault at all.

## Node pools

Worker nodes live in **node pools** (`domain.NodePool`, the `node_pools` child table): named,
independently-scalable groups of worker nodes, each at its own t-shirt size. `Cluster.Size` sizes the
**control plane** only. Every cluster is created with a `default` pool, but that is a starting shape,
not a fixture - once created it is scaled and deleted like any other, and a pool-less
(control-plane-only) cluster is legal.

The whole feature rests on one decision: **a node's pool is encoded in its VM name**
(`<cluster>-<pool>-<i>`, minted by `domain.DesiredNodes` - the single source of the desired node set,
naming *and* per-node sizing together). That is what keeps the reconcile loop pool-agnostic: scaling a
pool, adding one and removing one are all just "these VM names are (no longer) desired", which
`removedWorkers` + the provisioner's idempotent `EnsureNodes` already converge. **Deleting a pool
drains and destroys its nodes with no code of its own.** Anything new that needs the desired node set
must go through `DesiredNodes` rather than re-deriving names.

Four things are load-bearing:

- **Worker count is derived, never stored** (`Cluster.WorkerCount`). The pools are the single writer
  of worker topology; a cached total could only drift from them.
- **Nodes are no longer uniformly sized**, so anything that prices or measures a cluster must be
  per-node: `quota.ClusterUsage` walks `DesiredNodes`, and `Budget.Check` takes a **candidate
  cluster** rather than scalars (admission and provisioning derive the shape from one function, so a
  cluster that passes quota is the cluster that gets built). `metrics.Fake` uses `Cluster.NodeSize`
  per node for the same reason.
- **A pool's size and root-disk size are immutable**; the API rejects a change rather than silently
  rolling every node in it. The supported path is a new pool at the new shape, draining the old away
  (GKE/EKS node-group shape). `NodePool.DiskGB` overrides its workers' ROOT disk (0 = the t-shirt
  size's default, and it may only ever grow it - a node's volume is a COW clone of the golden image);
  control planes are never affected, being in no pool. For storage on a **running** node, the answer
  is a `NodeDisk`, not a bigger root disk.
- **Pool names are validated hard** (`domain.ValidatePoolName`): DNS-1123, unique, short enough that
  `<cluster>-<pool>-<i>` fits a 63-char hostname, and never `cp` - which would collide with the
  control planes' own names.

Pool membership is published as the node label `kaas.io/nodepool=<name>` (`domain.PoolLabel`) so pods
can target a pool with a nodeSelector. It is set at kubelet **registration** (`--node-labels` via
`/etc/default/kubelet`, written by the ansible `worker` role before `kubeadm join`) - registration is
the only time `--node-labels` is honoured, which fits because a node never changes pool. The prefix
sits outside `kubernetes.io`/`k8s.io` so the kubelet is permitted to self-set it; production would use
`node-restriction.kubernetes.io/nodepool` applied control-plane-side (see `docs/concepts/architecture.md`).

## Default storage (Longhorn)

Every cluster ships **persistent storage by default**: the **`longhorn`** add-on is in the bundle, and
every WORKER is born with an extra disk (`Cluster.StorageDiskGB`, 10 GB by default, chosen in the
create wizard) mounted at **`/var/lib/longhorn`** - Longhorn's own `defaultDataPath`. That single fact
is the whole mechanism: the chart's `persistence.defaultClass` makes Longhorn the cluster's **default
StorageClass**, so a plain `PersistentVolumeClaim` with no `storageClassName` gets a real, replicated
volume the moment the cluster is Ready, with nothing to configure and no hostPath in sight.

The feature deliberately adds **no storage concept of its own**. Longhorn wants a *mounted directory*,
not a raw device, which is exactly what `domain.NodeDisk` already delivers - so the platform's disk is
an ordinary `NodeDisk` (`domain.DesiredStorageDisks`, materialized under `LockAdmission` by
`app.syncStorageDisks` on create and on every pool edit), priced by the same quota, converged by the
same reconcile path, shown in the same Nodes-tab pane. Six things are load-bearing:

- **A disk's role is decided by WHERE IT IS MOUNTED**, not by a flag on the row
  (`NodeDisk.FeedsStoragePool`). A disk under `/var/lib/longhorn*` is pool capacity; a disk anywhere
  else is an ordinary filesystem Longhorn ignores - the escape hatch, and the shape every disk created
  before this feature has. A purpose column would be a second, invisible truth about something the
  user can already read off the mount path.
- **The platform's own disk is NOT registered with Longhorn** - it sits at `defaultDataPath`, so
  longhorn-manager discovers it unprompted, and registering it again is an *error* (Longhorn refuses
  two disks on one node sharing a path). The wiring step therefore handles only the ADDITIONAL disks a
  user attaches, and the common cluster runs no `ansible-playbook` at all.
- **Growth is another disk, not a bigger one.** A `NodeDisk`'s size is immutable and `StorageDiskGB` is
  fixed at creation; the portal's add-disk form defaults to `/var/lib/longhorn-<name>`, which
  `reconcile.reconcileStorageWiring` → `config.EnsureLonghornDisks` (the **`longhorn_disks`** role)
  patches onto that node's `node.longhorn.io` CR. Longhorn sums its disks' capacity, so each keeps its
  own VG and stays independently removable - no shared volume group, no `pvmove`.
- **`StorageWired` is a FINGERPRINT, not a bool** - unlike `GatewayWired`/`MonitoringWired`. Those
  guard work decided once at admission; disks come and go on a running cluster, and a bool would latch
  on the first one and silently strand every later one.
- **Eviction precedes unmount.** A registered disk holds volume replicas, so `releaseRemovedDisks`
  calls `EvictLonghornDisks` (the `longhorn-evict.yml` playbook: `evictionRequested` + drop the CR
  entry) BEFORE the guest teardown and the detach - the same drain-before-destroy shape as removing a
  worker. Reversing it degrades every volume with a replica there. The wait is bounded and gives up
  with a warning rather than wedging the cluster in `Updating`.
- **The replica factor is derived per cluster**: `domain.LonghornReplicas` = `min(3, workers)`, applied
  through `addons.Extras` (`app.longhornAddonExtras`) rather than the catalog, because a values
  override makes helm skip the catalog's `--set` entirely. The chart's default of 3 would leave every
  volume on a two-worker cluster permanently degraded; a flat 1 gives up surviving a lost node. It
  stands down when the user has edited the add-on's values - unlike external-dns's Extras, there is
  nothing here the platform must defend. It is fixed **at install**: a later scale-up does not raise it.

Bring-up ordering is load-bearing too: `mountNodeDisks` runs in **`PhaseWorkersReady`**, before
`PhaseInstallingAddons`, because Longhorn claims `/var/lib/longhorn` the moment it installs - if that
path were still the root disk, the cluster's storage would silently land there and the real disk would
never be used. The golden image already carries Longhorn's prerequisites (`open-iscsi`, `nfs-common`,
`cryptsetup`, `iscsi_tcp`; see the `common` role).

Longhorn's own console is proxied on the **Storage page** as a fourth `tunnel.App`
(`Surface: SurfaceStorage`). It is **write-scoped** like Alertmanager and more sharply - longhorn-ui
ships no auth and it deletes volumes. It is also the one app that **cannot be told its base path**
(`App.SelfPrefixed` false): longhorn-ui takes a single env var, `LONGHORN_MANAGER_IP`, and assumes it
owns `/`. So the agent serves it by inverting the tunnel path in both directions
(`internal/shell/agent/basepath.go`) - **strip** it from the request (Longhorn's nginx routes only `/`
and `/v1`; forwarding the tunnel path would fall through to its SPA catch-all and return index.html
for every asset), **restore** it in the response where the API server put its own proxy prefix, and
**inject a client-side shim** into the document head that wraps `fetch`/`XHR`/`WebSocket`/
`history.pushState` to re-base the root-relative URLs the SPA builds at runtime - the ones no
response rewriting can see. The shim's own CSP is dropped so the inline script runs. *Production
would* give the app a hostname of its own (`longhorn.<cluster>.<apps domain>` - the wildcard DNS and
default Gateway already make that trivial) and let it serve from the root it expects; this exists
because a lab control plane reaches clusters only through the API server's service proxy, where
per-app hostnames are not available. WebSocket-driven live updates may still not upgrade through that
service proxy (the same caveat as Grafana Live); the initial REST reads do.

*Production would* configure a real `backupTarget` (S3/NFS - snapshots are local-only here), and expect
poor throughput from replicated block storage on nested virt: this is a lab platform on a laptop.

## Automatic etcd maintenance

Long-term cluster management's second periodic operation, and deliberately the same shape as
certificate rotation: observed state on the cluster row, a time-driven due-list unioned into
`clustersNeedingWork`, one phase. The problem is specific - **compaction** (the apiserver's, every 5m)
frees etcd's keyspace *logically* but never shrinks the bbolt file; left alone it grows to its
high-water mark, and on reaching `--quota-backend-bytes` etcd arms **`NOSPACE` and the whole cluster
goes read-only** until someone defragments *and disarms it*. The existing `etcd quorum` health check
cannot see any of that: `/livez/etcd` stays green because every member is up and in quorum - they just
refuse writes.

Two halves. **Prevention** is static config and most of the value: the **`controlplane_etcd`** role
drops a kubeadm JSON6902 patch (the `controlplane_audit` mechanism) raising the quota to 8GiB and
turning on etcd-side periodic auto-compaction, applied before `init`, before every HA `join`, and -
along with `controlplane_audit`, which was missing there - before the **rolling-replacement** join in
`join-controlplane.yml`. **Remediation** is thresholded, never scheduled: `ClustersDueEtcdMaintenance`
surfaces Ready, converged clusters whose etcd is stale, the Ready tick calls `config.EtcdStatus` (one
read-only `etcdctl endpoint status --cluster`), and `domain.EtcdDefragPolicy.DefragDue` - a pure
function, so the whole decision is unit-tested without a cluster - decides. Six things are
load-bearing:

- **The absolute floor matters more than the ratio.** A 4MB store is routinely "200% fragmented";
  without `MinBytes` the platform would take a stop-the-world outage on every idle cluster forever to
  reclaim a few megabytes. The thresholds (45% over 100MiB) are OpenShift's etcd-defrag-controller
  numbers rather than invented ones.
- **A missing member is a hard refusal, and it outranks the emergency.** Defrag blocks the member it
  runs on, so doing it while another is unreachable is how a three-member cluster loses quorum. Every
  other condition decides whether the work is *worth* doing; this one decides whether it is *safe* -
  which is why it is checked both in the policy (member count < control planes) and again as the
  playbook's `endpoint health --cluster` pre-flight.
- **An armed `NOSPACE` bypasses the maintenance window and the hysteresis floor.** The cluster is
  already read-only; this is outage recovery, not hygiene, and waiting for Sunday is strictly worse.
- **Disarming is not optional.** Defragmentation alone does not restore writes on a cluster that hit
  its quota - the alarm stays armed until `etcdctl alarm disarm`.
- **Hysteresis, not a `…Wired` bool.** Unlike `GatewayWired`, "already defragmented" is never
  permanent: a cluster whose keyspace is genuinely large is still fragmented afterwards, and a bool
  would either latch forever or defragment on every observation.
- **The playbook is resumable, and the health gate runs on skipped members too.** The per-member ratio
  re-check is the counterpart of `renew_certs`' `cert_renew_cutoff_epoch`: a run killed by the job
  timeout resumes instead of re-bouncing every member. `serial: 1`, leadership moved off a member
  before defragmenting it, and the gate on *every* node is the quorum guarantee.

`PhaseDefragmentingEtcd` ranks **last** in the Ready switch, below cert rotation - both are periodic
maintenance, but an expiring certificate is a deadline and a defrag is discretionary.
`domain.MaintenanceWindow` (`KAAS_MAINTENANCE_WINDOW`/`_TZ`) is new and deliberately general: a defrag
is the first disruptive periodic operation the platform performs on a running cluster, not the last.
The health panel gains an `etcd backend store` check derived from the stamped status (degraded above
75% of quota - of the EFFECTIVE quota, so an untuned member is measured against etcd's 2GiB and is
reported rather than penalized: that state converges only when a defrag runs, and a degraded state
nothing converges is a permanently red panel), and the Monitoring page's etcd
section gains in-use and quota-usage panels. On a **single** control plane a defrag is a brief API
outage - the same trade the sole-CP etcd restore already makes. Taking a snapshot first on a sole
control plane is no longer a "production would" - `PhaseSnapshottingEtcd` ranks above the defrag, so a
cluster due for both backs up first (see *Periodic control-plane backups*). *Production would* still
alert on quota headroom from a real metrics sink rather than a portal panel.

## Periodic control-plane backups (etcd snapshots)

Every Ready cluster is backed up on a cadence (`KAAS_ETCD_SNAPSHOT_INTERVAL`, 6h): an **online**
`etcdctl snapshot save` of the keyspace **plus `/etc/kubernetes` and `/var/lib/kubelet`**, sealed and
stored in Postgres (`domain.EtcdSnapshot`, the `etcd_snapshots` table, `PhaseSnapshottingEtcd`). It
exists for one fault nothing else can reach: **a sole control plane whose VM is unrecoverable.**
Everywhere else the platform copies state off a live node before replacing it
(`backup-controlplane.yml`) or leans on a surviving quorum member - both assume something is still
running. Same time-driven shape as cert rotation and defrag: `ClustersDueEtcdSnapshot` unioned into
`clustersNeedingWork`, a scalar `etcd_snapshot_at` on the row driving it.

Six things are load-bearing:

- **It is NOT `backup-controlplane.yml` on a timer.** That play takes a raw copy of `/var/lib/etcd`,
  which is only consistent with etcd stopped - so it removes the etcd manifest, waits for the port to
  close, and stops kubelet. Correct there (the node is destroyed immediately after), fatal on a
  cadence: it would stop the control plane of every healthy cluster every few hours. `snapshot save`
  streams a consistent view while etcd keeps serving, which is what makes a periodic backup possible.
- **The PKI rides along, and is not optional.** A keyspace snapshot alone cannot rebuild a control
  plane: without the original CA key the restored apiserver serves a different CA and every kubelet
  cert, every per-user kubeconfig and every cert-subject-keyed binding stops verifying. Restoring half
  the state is a differently-broken cluster, not a partial recovery.
- **A snapshot is the entire cluster's Secrets in plaintext plus the CA private key** - strictly more
  sensitive than `SecretKubeconfig`, which is only a credential *to* the cluster rather than a copy
  *of* it. So it is sealed with `secrets.Box` **before** it reaches the store, unpacked only into the
  worker's own artifacts dir and deleted after, wiped off the node at the end of the play, and exposed
  through **no API surface** - there is no download endpoint and the portal never sees more than
  metadata. A "download backup" button would hand any cluster owner an offline copy of every Secret
  their cluster has ever held.
- **It lives in Postgres because worker replicas are stateless.** A snapshot on the node is worthless
  (the node is what died); a snapshot on one worker's disk violates *"nothing is pinned to a replica"*
  - the replica that has to restore is not the one that took it. Postgres is the only durable store
  every replica shares. Capped by `domain.MaxEtcdSnapshotBytes`; over it the platform refuses rather
  than putting a multi-gigabyte parameter on one INSERT.
- **An unverifiable snapshot is never stored.** The play reads the file back (`etcdutl snapshot
  status`) and a revision of 0 fails the step. A corrupt backup is worse than none: it satisfies
  retention, silences the staleness health check, and fails at the only moment it is used.
- **The cadence marker moves only after the store write succeeds.** Stamping it earlier would push the
  next attempt a full interval away on failure, halving the coverage of exactly the cluster that can
  least afford it. Retention (`PruneEtcdSnapshots`) is the mirror image - best-effort, never an error,
  and it clamps `Retain` to at least 1, because a retention policy that deletes the last backup is not
  one.

Restore (`restore-etcd-snapshot.yml`) is a **different play** from `restore-controlplane.yml`: a
snapshot is not a data directory, it has to be materialised by `etcdutl snapshot restore`, which must
be told the member's identity (`--name`, `--initial-cluster`, `--initial-advertise-peer-urls`) because
a snapshot carries none. `etcdutl` lives only inside the etcd image and there is no cluster left to
`kubectl exec` into, so the image is run directly through `ctr` - its tag read from the just-restored
static-pod manifest, so the restore uses the same binary that wrote the snapshot. The health panel
gains an `etcd-backup` check (degraded past 2× the interval) because the classic way a backup fails is
silently. *Production would* ship the payload to object storage under a KMS-backed key with its own
retention and audit policy.

## Automatic cluster and node repair

The platform keeps clusters **operational**, not merely provisioned: a Ready cluster whose node has
gone NotReady, whose VM is powered off, or which never joined is detected, escalated through a ladder
of increasingly expensive repairs, and rebuilt if nothing cheaper works (`domain.RepairPolicy`,
`internal/reconcile/repair.go`, `PhaseRepairing`).

The gap it closes is narrow and specific. The platform **already detected** all of this
(`internal/health`) and deliberately did nothing: health is decoupled from the state machine and never
changes a phase, and a Ready cluster is not in `ClustersNeedingWork`, so nothing ever acted on the
detection. And the **repairs themselves already existed** - `RemoveWorkers` already drains from the
control plane and tolerates a dead node, `rollOneNode` already rebuilds any node HA-aware and
non-destructively to its extra disks. What was missing was durable per-node state, a guarded policy,
and a phase. So this is decision-making, not new ways to touch a cluster.

**`domain.ClusterRepair` (one JSONB column) exists for one word: DURATION.** `cluster_health` keeps
only the newest snapshot, so "this node is NotReady" is readable and "this node has been NotReady for
twenty minutes" is not - and every safe repair decision is the second sentence. `UnhealthySince` is
that sentence.

The ladder, cheapest first, each rung an operation the platform already performs: add-on/CNI reinstall
→ **power-on** (`provision.NodePowerer`) → **rejoin** (the idempotent join) → **restart-kubelet** (the
`node_repair` role) → **replace** (`replaceNode`, the rolling-replacement machinery re-pointed at the
node's *current* image) → **restore** (a sole control plane from a stored snapshot; the only lossy
action, its own gate).

**The guards are the feature.** Seven are load-bearing:

- **A cluster you cannot see is not a cluster that is broken.** When the API server is unreachable -
  or the health snapshot is older than `HealthMaxAge` - every node reads NotReady, and the honest
  conclusion is *I cannot see*, not *rebuild everything*. Acting on the second reading is how a
  platform destroys a healthy cluster during a network partition.
- **…with exactly one exception, and it is why the infrastructure layer is consulted at all.** A VM the
  *hypervisor* says is off is a fault observed **below** the layer that has gone quiet, from an
  independent source. That corroboration is what makes control-plane repair possible: on a
  single-control-plane cluster the API server going away *is* the symptom, so a policy that always
  refuses when it is unreachable could never fix the case it exists for.
- **Blast radius, per cluster** (`MaxUnhealthyFraction`, 0.5): past it this is one cluster-wide fault
  wearing N masks, not N node faults. It requires **at least two faults** to trip - a single fault has
  no "many" to infer a shared cause from, and without that clause a sole-CP cluster is 100% faulty the
  instant its only node breaks and could never be repaired.
- **Blast radius, per fleet** (`MaxUnhealthyClusters`): the guard no per-cluster check can replace.
  When the worker loses the hypervisor or the tunnel, every cluster goes unhealthy at once and each
  looks *locally* like an ordinary repairable fault. This is what stands between a dropped VPN and a
  rebuilt estate.
- **Quorum outranks the emergency.** Never replace a control plane while another etcd member is
  unreachable - the same refusal, for the same reason, as `EtcdDefragPolicy.DefragDue`.
- **Give up loudly.** `MaxAttempts` then `Suspended`, with exponential backoff between, and attempts
  **carried across a flap** so recovery is not an unlimited supply of fresh ones. A repair loop is
  worse than the fault: it burns host capacity and hides the real cause. Suspension raises the
  `auto-repair` health check to unhealthy - self-healing has stopped and a human is needed.
- **The attempt is recorded BEFORE the work**, never after - the same ordering contract as
  drain-before-destroy. A counter that only advances on success never gives up. It is also why
  `Target`/`Action` are persisted rather than re-derived: the Ready tick decides and `PhaseRepairing`
  executes, on possibly different replicas, and re-deriving would land on a different rung than the
  one announced. Symmetrically, `CompleteAttempt` does **not** clear the fault - whether a repair
  worked is decided by the next *observation*, not by the action returning, or a kubelet restart that
  changed nothing would look like success and the ladder would never escalate.

Two more, structural: repair ranks **below upgrade/update** in the Ready switch (those are the user's
intent, and both drain and rebuild nodes on purpose - every one of which looks exactly like these
faults) and **above** certs/snapshots/defrag (those are deadlines and hygiene on a working cluster;
this runs when a cluster is not). And a failed repair **does not fail the reconcile step** - it is an
ordinary outcome of repairing a broken thing, and returning it would hand the job to River's backoff,
re-running the same rung outside the policy that decided it was due.

`provision.NodeReplacer` is the one genuinely new mechanism. `EnsureNodes` is *converge*, so a VM that
exists but is broken already matches its spec; the OS upgrade only sidesteps this because a changed
image forces the root volume's replacement (on libvirt, via the `terraform_data.node_volume_shape`
trigger the module carries - see below). It is implemented as `tofu apply -replace`, and on libvirt it
names the **root volume as well as the domain** - replacing the domain alone re-attaches the same copy-on-write root
disk, repairing nothing - while deliberately **excluding the extra disks**, which hold the node's data
and its Longhorn replicas. That extra-disk preservation is now **uniform across all three providers**,
because each keeps a node's extra disks in a resource INDEPENDENT of its VM: a `libvirt_volume`, a
`vsphere_virtual_disk` attached with `attach = true`, or a volume on the per-cluster Proxmox
**disk-owner VM** attached by `path_in_datastore` (see *Node disks*). So a `-replace` of the node's VM
rebuilds it and re-attaches the same disks - a disk-bearing node is no longer refused on any backend,
and there is no disk-preservation preflight left in the reconciler. (Neither vSphere nor Proxmox
implements `NodePowerer` - it would mean a second, differently-authenticated path to the backend - so
those two still lack the cheapest, power-on rung, which is a different limitation.) The fake implements
`NodeReplacer` + `NodePowerer`, so `up-fake` demos the whole ladder.

## Node disks

Beyond its root disk, a worker can carry **extra disks** (`domain.NodeDisk`, the `node_disks` child
table): named block devices attached to ONE node, LVM-formatted and mounted by the `node_disks`
Ansible role. `NodePool.DiskGB` sizes a pool's root disk at creation; a `NodeDisk` is what gives a
**running** node more storage, non-destructively. Portal: click a node on the Nodes tab → a right-hand
detail pane (`web/portal/src/components/NodeDetailPane.tsx`), which is deliberately the home for
future per-node settings rather than more columns in the node table.

Six things are load-bearing:

- **Disks are keyed on the node's VM NAME**, not a node ID - a node row is observed state and is
  re-created whenever its VM is, so the name is the stable identity (same reasoning as `StaticIPs`).
  A node rebuilt by a rolling OS replacement keeps its disks, and the role **re-adopts** the existing
  LVM rather than reformatting (every step is guarded; mkfs by a `blkid` check).
- **The device is resolved by hardware identity, never a kernel name.** Guest names renumber on
  detach, so `NodeDisk.WWN` (kvm + proxmox, minted at admission - pinned as the virtual disk's wwn on
  kvm, as its **serial** on proxmox) / `DeviceID` (observed; vsphere's VMDK UUID via
  `enable_disk_uuid`) is what Ansible matches under `/dev/disk/by-id/`. Nothing is formatted before
  the provisioner has *observed* an identity.
- **Removal is release-then-detach.** `DELETE` flips the disk to `removing` and the ROW is what keeps
  the volume attached; the reconciler unmounts + `vgremove`s in the guest, THEN drops the row, which
  lets the next `EnsureNodes` destroy the volume. Same shape as drain-before-destroy. Reversing it
  strands a mount over a dead device.
- **An extra disk's storage is a resource INDEPENDENT of the node's VM, on every provider** - so a
  node's VM can be rebuilt (a repair, a rolling OS replacement) and its disks, and their data, are
  untouched and re-attached. That is why the first bullet's "keeps its disks" holds uniformly. The
  three do it differently: kvm a `libvirt_volume` per disk; vSphere a standalone `vsphere_virtual_disk`
  attached with `attach = true`; Proxmox a volume on the per-cluster **disk-owner VM** (a never-started
  VM that exists only to own the volumes, since bpg has no standalone disk resource) attached to the
  node by `path_in_datastore`. Tofu owns the volumes on all three; only the node↔disk *attachment*
  varies.
- **On kvm, that attachment is converged with `virsh`, not OpenTofu** - OpenTofu can only converge a
  domain's device list by REDEFINING the domain, which writes libvirt's persistent XML and leaves the
  running QEMU process untouched, so "attach storage to this worker" would do nothing until the node
  rebooted (and on the old 0.8 provider, worse: the disk list was ForceNew, so it destroyed the node).
  The module declares the disks (so a replaced domain comes back with them) but `ignore_changes =
  [devices]`, and `internal/provision/tofu/disks.go` diffs `virsh dumpxml` and hot-attaches
  `--live --persistent`, which updates both copies at once. For that attach to be possible at all the
  module declares a **virtio-scsi controller on every node, whether or not it has disks**: libvirt adds
  one implicitly only for a *declared* scsi disk, so hot-attaching a first disk would have to hot-add
  the controller too - a PCI hotplug i440fx refuses - and the feature would fail on exactly the nodes
  with no storage yet. **Ordering
  around `apply` is load-bearing**: a volume must exist while its disk is attached, so `EnsureNodes`
  DETACHES removed disks before apply (while their volumes live), applies, then ATTACHES new ones - and
  `DestroyCluster` sweeps disks off every domain before `tofu destroy`. Detach-after-apply (or a
  skipped detach) strands a domain pointing at a deleted volume, which wedges every later refresh
  including destroy. vSphere and Proxmox need none of this virsh dance - they hot-add/remove the
  attachment in place declaratively. Real libvirt regression: `disks_libvirt_test.go`, gated on
  `KAAS_TEST_LIBVIRT=1`.
- **Disk is a quota dimension** (`ResourceQuota.DiskGB`, `KAAS_BUDGET_DISK_GB`): root disks + every
  extra disk, charged in EVERY phase including `removing` (the volume exists until it is actually
  destroyed). A pool scale-down prunes its departing nodes' disks (`app.disksOnDesiredNodes`) - a
  stale row is state nothing converges, and `ValidateNodeDisks` rejects it, wedging later edits. The
  per-worker storage disk above is charged the same way, so the default cluster now costs
  `StorageDiskGB × workers` more than its root disks.

## Infrastructure providers

A cluster runs on **one infrastructure**, recorded immutably on the cluster row (`domain.Cluster.
Provider`: `kvm` | `vsphere` | `proxmox`) at create time. `KAAS_INFRA_PROVIDERS` (default `kvm`)
lists what a deployment offers - the portal's wizard shows an **Infrastructure** step only when
there's more than one - and it is **orthogonal to `KAAS_PROVISIONER`**, which stays the fake/real
axis: in fake mode every provider name maps to one shared `provision.Fake`, so `make up-fake` demos
the whole flow with no KVM, no vCenter and no Proxmox.

**vSphere and Proxmox are the same KIND of provider** - VMs clone a golden **template** onto the
operator's **shared** network (a portgroup / a bridge), node addressing is external DHCP (read back
from the guest tools/agent) or platform-allocated static, and the worker reaches the backend
**directly**. So they share one deployment-level config shape (`sharedNetSettings`) and one admission
path (`admitSharedNetwork` / `scaleSharedStaticIPs`). KVM is the odd one out: a dedicated per-cluster
network. Read the "vSphere" invariants below as "any shared-network provider" unless noted.

The reconcile loop stays provider-agnostic: it holds a `map[provider]Provisioner` and dispatches
through `Reconciler.prov(c)`; orphan GC sweeps **every** provisioner (deduped by identity), since the
cluster row that would name the backend is exactly what's gone. Everything provider-shaped is decided
**once, at admission** (`internal/app`, under `store.LockAdmission`) and written to the cluster row -
the loop never reads it from env.

Where they differ, and what is load-bearing:

- **Network.** KVM: an isolated NAT bridge per cluster, CIDR from `internal/netpool`. vSphere/Proxmox:
  the operator's **shared** network (a portgroup / a bridge; deployment config). So per-cluster CIDR
  exclusivity is now a kvm-only invariant - the `clusters_network_cidr_live` index is scoped to
  `provider='kvm'`, and the exclusive resource on a shared subnet is the HA VIP, backstopped by a
  per-provider partial index (`clusters_vsphere_vip_live`, `clusters_proxmox_vip_live`).
- **Addressing.** `ip_mode=dhcp`: the site's DHCP server assigns node IPs - read back via open-vm-tools
  (vSphere) or the **QEMU guest agent** (Proxmox, baked into the golden image by Packer). **Pinned
  deterministic MACs** (VMware's `00:50:56:00–3f` range; Proxmox's `BC:24:11` OUI) make a re-created
  node reclaim its lease. `ip_mode=static`: the platform allocates node IPs + VIP from a range and
  injects them via cloud-init (guestinfo netplan on vSphere; native `ip_config` on Proxmox),
  persisting them per `(cluster_id, vm_name)` in `clusters.static_ips`. **Either way the
  stable-IP-on-recreate contract must hold** - rolling OS replacement and sole-CP etcd restore both
  key on the node IP.
- **HA VIP.** KVM derives it from the cluster's own subnet. vSphere/Proxmox `dhcp`: the **user**
  supplies it (nothing else knows what's free outside the DHCP pool); `static`: allocated.
- **Node name vs. guest hostname.** kvm/vsphere author their own cloud-init metadata and set the
  guest hostname to the bare VM name (`n.name`, e.g. `test-cp-0`); Proxmox VMs are named
  `<cluster_id>-<n.name>` for PVE-wide uniqueness (tenants can both name a cluster `dev`), and
  Proxmox's *native* cloud-init (no hand-authored metadata) derives the guest hostname straight
  from that VM name - so without correction the k8s node registers as
  `<cluster_id>-test-cp-0`, breaking every kubectl/Ansible lookup keyed on `inventory_hostname`
  (worker joins, the upgrade role's drain/uncordon, node_disks). Fixed uniformly, not just for
  Proxmox: every `kubeadm init`/`join` passes `--node-name {{ inventory_hostname }}`, so the k8s
  node name is always the platform-minted VM name regardless of what the guest OS hostname is -
  a no-op on kvm/vsphere, load-bearing on Proxmox.
- **Golden image.** `catalog.GoldenImageNameFor(provider, os, k8s)` - a qcow2 on KVM, a VM **template**
  (no suffix) on vSphere and Proxmox, built by `make golden-image-{vsphere,proxmox}` from the same
  `ansible/playbooks/golden-image.yml`. On Proxmox the module resolves the template NAME → numeric
  vm_id (a `proxmox_virtual_environment_vms` data lookup) to clone it.
- **Extra disks (`NodeDisk`).** Stable device identity comes from a platform-minted `WWN` on KVM
  (pinned on the virtual disk) and Proxmox (set as the disk **serial** → `/dev/disk/by-id/scsi-…`),
  both known up front; vSphere reads back the VMDK UUID (`DeviceID`). One `node_disks` Ansible role
  resolves the device by matching the reported token, so nothing above the provision seam is
  provider-aware. On all three the disk's STORAGE lives in a resource independent of the node VM (a
  `libvirt_volume`, a `vsphere_virtual_disk`, a volume on the Proxmox disk-owner VM), so a node rebuild
  preserves it - see *Node disks*. vSphere and Proxmox hot-add/remove the *attachment* declaratively,
  in place (unlike libvirt's `virsh` dance).
- **Reachability.** The worker reaches vCenter/Proxmox and the cluster VMs **directly** (no tunnel),
  so `KAAS_KVM_HOST` - whose SSH/SOCKS rerouting is global - **cannot be combined with vSphere or
  Proxmox**; the app refuses to start if both are set.
- **Quota.** Quota is **per-user, per-infrastructure** (`domain.User.Quotas`, a `provider →
  {vcpu, mem_mb}` map). Each provider has its own ceiling (`KAAS_BUDGET_*` = the KVM host,
  `KAAS_VSPHERE_BUDGET_*` = vCenter, `KAAS_PROXMOX_BUDGET_*` = the Proxmox cluster) and its own
  conserved pool; there is **no summed platform total**, because capacity isn't fungible - KVM
  headroom can't fund a vSphere VM. Admission charges the owner's grant on the cluster's own provider.
- **Secrets.** Backend credentials go into the tofu process env (`VSPHERE_*` / `PROXMOX_VE_*`), never
  into the workspace's `terraform.tfvars.json`. Proxmox accepts **either** an API token
  (`KAAS_PROXMOX_API_TOKEN`) **or** a username/password (`KAAS_PROXMOX_USERNAME`/`PASSWORD`) - exactly
  one. The API container never gets them - it does admission, not provisioning.

**NetBox** (`internal/netbox`, opt-in via `KAAS_NETBOX_URL`, **shared-network providers only** -
vSphere and Proxmox, via `maybeWrapNetbox`) is a decorator around the provisioner: it **records** each
cluster's node IPs + HA VIP in the site IPAM as they are learned and releases them on destroy. It
records, it does not allocate. Writes are idempotent upserts keyed on the address, scoped by our tag +
a `kaas:<cluster-id>` marker so an existing hypervisor→NetBox sync coexists and we only delete what we
created. A NetBox failure **fails the reconcile step** (it retries and converges) - silently skipping
registration would let the IPAM drift, which on a shared subnet is what hands one address to two
machines. It is never wrapped around KVM: a per-cluster private network is nobody else's business.

## Horizontal scaling

Every stateless tier - `web`, `api`, `worker`, the exec-agent sandbox - runs with **N replicas**
(`make up-scale WEB=3 API=3 WORKER=4`; `deploy/compose.scale.yaml`, where an `lb` container owns the
published ports and fans out by DNS). Postgres stays single (deliberate; see `docs/concepts/architecture.md`).
Four rules keep it correct, and **new code must not break them**:

- **Reconcile work is claimed, not assigned.** River's job uniqueness is on `ClusterID` across the
  active states, so one job per cluster runs platform-wide however many workers exist. Any new
  per-cluster background work belongs in the queue, not in a ticker.
- **Singleton loops are leader-elected** (`internal/reconcile/leader.go`, a `pg_advisory_lock`
  lease). The GC/metrics/health sweeps are plain tickers and GC *destroys infra* - anything similar
  (a periodic sweep, a cron-ish job) must run under the leader lease, never once per replica.
- **Read-then-write admission is locked** (`store.Store.WithLock` → advisory lock; `LockAdmission`
  guards quota + `netpool` IPAM, `LockUserSeed` the admin seed; the migrators use the same
  primitive). Any new check that reads the world and then writes to it needs the same treatment,
  plus a schema-level backstop where possible (`clusters_network_cidr_live`).
- **Nothing is pinned to a replica.** Signed-cookie sessions, Postgres `LISTEN`/`NOTIFY` for events,
  stateless exec agents (`internal/execagent` round-robins `KAAS_SHELL_AGENT_ADDR`) - so no sticky
  routing anywhere. Don't add in-process state that a request on another replica would need.

## Browser demo (the control plane as WebAssembly)

The platform publishes itself as a **static site** on GitHub Pages: the portal plus the whole control
plane compiled to `js/wasm` and running in the visitor's tab (`cmd/demo-wasm`, `web/portal/src/demo/`,
`.github/workflows/pages.yml`; operator-facing guide in
[`docs/deploy/browser-demo.md`](docs/deploy/browser-demo.md)). It is **not a mock**: it is `cmd/api`
in a different wrapper - the same `internal/app`, the same `api.Routes()`, the same reconcile loop and
state machine - against the in-memory store and the fakes `make up-fake` already uses. That is the
whole reason this is cheap: **the seam table above is the feature**. Nothing was stubbed for it.

Five things are load-bearing:

- **`internal/app` is untouched, and must stay that way.** Only four packages cannot compile for
  `js/wasm`: the three exec-agent proxies (`opts.HTTPHeader` - a browser WebSocket handshake carries
  no headers) and `internal/shell/pty` (`creack/pty` syscalls). Each got a build-tagged counterpart
  (`execagent.DialOptions`, `pty_js.go`) rather than a `//go:build js` fork of the 4000-line app
  wiring, which would have made every future seam a two-place edit. CI builds the wasm target on
  every PR, because nothing in an ordinary build exercises those files.
- **The terminals' session logic lives in `internal/api/session.go`, not in their handlers.** A
  browser has no connection to hijack, so `websocket.Accept` cannot run and the demo drives a
  `shell.Conn` of its own. Splitting the post-upgrade half out (untagged - the real handlers are the
  primary caller) is what stops the demo from carrying a parallel copy of the not-Ready gating, the
  per-user kubeconfig minting and the node-SSH auditing. Authorization deliberately stays in the
  callers: it happens *before* the upgrade so an unauthorized request gets an HTTP status.
- **The shim patches three browser APIs and nothing else** (`fetch`, `EventSource`, `WebSocket`).
  `lib/api.ts` and every page are unchanged and unaware. Two non-obvious consequences: a browser
  ignores `Set-Cookie` on a JS-constructed `Response`, so **the session cookie is held in the shim**
  and re-attached as an ordinary header; and Go's wasm scheduler runs a newly spawned goroutine
  *before* returning to the JS caller, so bridge calls are **deferred by a microtask** - otherwise the
  first callback fires during `new EventSource(...)`, before the caller has assigned `onopen`. Both
  were real bugs, not hypotheticals.
- **A page-hosted module, not a service worker.** A service worker would make the tunnel links
  ordinary navigations, but it is terminated when idle - which stops the reconcile loop and discards
  the store. For a demo whose point is a control plane that keeps converging while you watch, that is
  disqualifying. The cost is the two surfaces that open a tab (the "Open UI" links and the Vault
  handoff), handled in the shim: the first is served from the module through a blob URL, the second
  explains itself.
- **The seed goes through the ordinary app API** (`CreateCluster`, `UpdateUser`, `AddNodeDisk`) and
  waits for convergence, rather than writing rows. What a visitor lands on is a fleet the platform
  actually built, with real phases, real events, real quota charged - a fixture would be the one part
  of the demo that isn't the product. `KAAS_RECONCILE_INTERVAL` exists for it (the tick loop only;
  River drives the real path).

*Production would* persist the store to IndexedDB so a reload keeps the fleet, and build-tag out the
Postgres store, govmomi, LDAP and WinRM - about a third of the 46 MB module, and unreachable in this
build anyway. It ships pre-compressed (~8 MB) because Pages will not negotiate an encoding for
`application/wasm`.

## Working in this repo

- Build/test: `make build`, `make test`, `make vet`. Run locally with `make run-api` /
  `make run-worker` (fake providers). Containers: `make up-fake` (no KVM) or `make up` (real).
  The static browser demo: `make demo-dev` / `make demo-build` (see *Browser demo*).
- The reconciler advances **one phase per invocation** (`reconcileOne` in `internal/reconcile`);
  phases and the state machine live in `internal/domain`.
- Ansible uses only `ansible.builtin`, so `ansible-playbook --syntax-check` works without extra
  collections. The OpenTofu module validates with `tofu init -backend=false && tofu validate`.
- JSON is `snake_case` (domain types carry the tags); the portal (`web/portal/`) consumes it
  under `/api` (nginx in containers, Vite proxy in `make web-dev`). The API serves JSON + SSE
  only - it has no HTML routes.
- Releases are **tag-driven**: `v1.4.0` (the five images), `chart-v0.3.0` (the Helm chart),
  `worker-v1.4.1` (one image, the hotfix path). The root `VERSION` file is the platform version's
  single source of truth; `make release-check` (run by CI on every PR) is the backstop. Never bump a
  mirror by hand - `make bump VERSION=x.y.z`. See *Releases* below.
- Commit only when asked; branch off `main` first if you do.

## Releases

Releases happen by **pushing a git tag** and nothing else - no release button, no self-publishing
branch, no step that runs from a laptop, so the tag is the record of what shipped
(`.github/workflows/release.yml`, `release-chart.yml`; operator guide in
[`docs/deploy/releasing.md`](docs/deploy/releasing.md)). Five images and the chart go to GHCR; the
`lb` image is compose-only and deliberately unpublished. Six things are load-bearing:

- **One platform version across all five images, published as five INDEPENDENT packages.** `api`,
  `worker`, `shell` and `nodessh` are one Go module sharing `internal/` - the exec-agent wire
  protocol, the store, the domain types - so a deployment running two of them at different versions
  is a hazard, not a convenience, and independent semver per image would make that the default state.
  What "individually" buys is real anyway: each is its own pullable, pinnable package. The one
  exception is the `<component>-v*` **hotfix tag**, consumed by the chart's `image.tags.<component>`
  override, and it is documented as temporary - fold it into the next platform release.
- **The chart floats free** (`chart-v*`), because the deployment surface changes on a different
  rhythm: a template fix is a chart release with no image rebuilt, and most platform releases need no
  chart change. What ties them is the chart's **`appVersion`**, which `kaas.image`
  (`templates/_helpers.tpl`) resolves every image tag from - so `values.yaml`'s `image.tag` must stay
  **EMPTY**. It used to be `latest`, which shadowed `appVersion` entirely and made the chart both
  unreleasable and silently pinned to a moving tag.
- **`VERSION` is a single source with a CI backstop, not a documented checklist.** Three files carried
  a version with nothing tying them together; the chart's `appVersion` is load-bearing, so drift there
  publishes a chart pointing at images nobody built. `scripts/version.py` owns the rule and is called
  from three places - `make release-check`, the CI `version` job, and the release workflow's tag guard
  (`--check <expected>`) - rather than reimplemented in each. Same discipline as
  `clusters_network_cidr_live`: a convention gets a mechanism.
- **The release re-runs CI by CALLING `ci.yml`** (`workflow_call`), never by copying its jobs. A
  release must not be able to pass a weaker gate than an ordinary PR.
- **GitHub Deployments are RECORD-ONLY**, and the `release` environment is an approval gate in front
  of every publish. The platform runs as Podman containers inside WSL2, which a GitHub-hosted runner
  cannot reach - and a CI credential that *could* reach it would put a GitHub-triggered process next
  to the libvirt socket, `KAAS_SECRET_KEY` and every tenant's Vault token. The pipeline holds nothing
  but a repo-scoped `GITHUB_TOKEN`. A real deploy job is a documented seam (a self-hosted runner),
  not a wired one.
- **A prerelease publishes only its exact version** - never `latest`, never the `X.Y` alias. Moving
  either would hand an rc to everything tracking them.

Build identity is stamped, not guessed: `internal/version` is filled by `-ldflags -X` from the
Makefile and the Containerfile `ARG`s, surfaced on the public `GET /version` (public for the same
reason `/healthz` is, and carrying version/commit/date only - which release is running is not a
secret, the machine that built it is), logged at start-up by both binaries, and shown in the portal's
sidebar footer. The portal READS it from the API rather than baking in its own `package.json`
version, because web and api are separate images that legitimately differ mid-upgrade. Published
digests carry a keyless `attest-build-provenance` attestation. Images are `linux/amd64` only - the
worker bakes OpenTofu with three provider plugins plus Ansible/Helm/kubectl, and cross-building that
under QEMU would dominate the pipeline for a platform nobody runs; adding arm64 is one line in
`platforms`. *Production would* also sign with cosign and publish an SBOM per image.

## Hard environment constraints

- Everything runs in **Podman containers inside WSL2**; KVM/libvirtd works in WSL2.
- The **worker** is the only component that touches KVM: it runs `--network host` with the
  libvirt socket mounted (`-v /run/libvirt:/run/libvirt`). The API and Postgres stay on a normal
  Podman network. See [`docs/deploy/providers/libvirt.md`](docs/deploy/providers/libvirt.md).
- The hypervisor is local **by default, not by design**. `KAAS_KVM_HOST` (`internal/kvmhost`) points
  the platform at a **remote KVM host**: OpenTofu dials it over `qemu+ssh`, Ansible reaches the VMs
  with an ssh `ProxyCommand` through it, and kubectl/helm go through an ssh SOCKS tunnel injected
  into the kubeconfig as `proxy-url` (`internal/kubeconfig`) - so every seam works unchanged. Unset,
  every one of those is a no-op. Anything new that must reach a cluster VM goes through `kvmhost`.
- Guard host capacity - VMs oversubscribe a laptop fast. Capacity quota (`internal/quota`) is a
  first-class admission check; an HA cluster is charged for all 3 control planes. Nodes are **not**
  uniformly sized (see *Node pools*), so a cluster is priced by walking `domain.DesiredNodes` - never
  by multiplying one t-shirt size by a node count.

## Fidelity stance: *balanced*

Build the load-bearing patterns for real (reconciliation loop, state machine, idempotency,
durable retries, real provisioning/config/add-ons, HA control planes, **bundle upgrades** - a
component-diff dispatch: in-place `kubeadm` for a Kubernetes minor, rolling node replacement for an
OS change, `helm upgrade` for add-ons - event streaming, orphan GC, capacity quota, at-rest secret
encryption, **multi-tenancy** (local accounts, per-user **per-infrastructure** quota under a
conserved-pool invariant enforced per backend - the admin holds no fixed slice on any of them, its
budget on each is always that backend's live unallocated pool - owner-scoped
clusters, and admin-managed **groups** whose members share access to each other's clusters under a
coarse **read/write role** that is scoped **per group** - a user can belong to several groups at once
and hold a different role in each (Read = view-only plus a **read-only kubeconfig + shell**; Write =
full management. Every interactive kubectl surface - the **download**, the **shell**, and the
**Workloads/Storage/scale** seams - runs as a **per-user** credential (`app.userClusterKubeconfig` →
`kube.MintUserKubeconfig`, memoized in `app.ukcCache` so a page of calls mints once): a
cluster-CA-signed client cert carrying the actor's own login as CN and their resolved role as a
Kubernetes group (O=`kaas:writers`|`kaas:readers`, `domain.KubeGroupForRole`), which the
`viewer_kubeconfig` role's ClusterRoleBindings map to `cluster-admin`/`view` (the latter reusing the
`config.EnsureViewerKubeconfig` cluster-read role) - so cluster API access is tied to the **same
identity+role the portal resolves**, uniformly for local and LDAP accounts, and the API-server audit
trail records the real username instead of a shared `kubernetes-admin`. Bounded by
`KAAS_USER_KUBECONFIG_TTL` (client certs aren't revocable before expiry, like the session cookie). The
platform's own **admin** kubeconfig (`SecretKubeconfig`) is still the server-side credential for the
non-kubectl-as-user query seams - Monitoring/Security/Audit/tunnel run curated read-only queries the
`view` role can't express, so those keep using admin server-side, gated by the API. Admin-assigned,
Read the least-privileged default; a user always keeps full control of clusters they own; `accessTo`
takes the highest role across shared groups); `internal/auth`,
`internal/domain.User.Memberships`/`Group`/`GroupMembership` (the `group_memberships` join table) -
the reconcile loop stays untenanted), and **directory authentication** against Active Directory /
LDAP (`internal/authn`: a service-account bind, per-rule group mapping, just-in-time account
provisioning, and a throttle in front of the public login endpoint), and a **central HashiCorp Vault
secret store** (`internal/vault`: per-cluster KV paths + policies mirroring the portal's read/write
model, kept converged with Postgres, consumed in-cluster by the bundled external-secrets add-on and
surfaced on the Secrets page - see *Secret store*). Deliberately
stubbed, and marked in code with a "production would…" note: secret key management (env-derived AES
key for the platform's own at-rest encryption, not a KMS; the LDAP bind password likewise comes from
the env; and the central Vault runs in dev mode - auto-unsealed under a fixed root token, in-memory
storage - rather than HA Vault with KMS auto-unseal and an audit device); authn (local accounts
with bcrypt + signed-cookie sessions - no IdP/OIDC, no server-side revocation, env-derived signing
key) and coarse authz (an owner-or-admin split plus a group read/write role, not fine-grained
per-resource RBAC); **directory deprovisioning** - sessions are stateless and `ResolveSession` never
re-checks the directory, so disabling a user in AD does not evict them until their token expires
(≤24h), and their platform row outlives the directory account entirely because nothing deletes it
(production: server-side revocation + SCIM or a periodic sync under the leader lease); and platform
HA/TLS. OS upgrades **replace** nodes (per-node golden-image swap): HA control planes roll one node
at a time keeping etcd quorum, and a *single* control plane is rebuilt via etcd backup/restore onto
the same IP (a pinned MAC), so it survives an OS change with a brief outage. **Keeping clusters
operational is the platform's job, not the tenant's**: periodic sealed etcd snapshots and a guarded
automatic-repair ladder (detect → power-on → rejoin → restart kubelet → rebuild the node → restore a
sole control plane from a backup) are built for real, with the refusal conditions - blast-radius caps
per cluster and per fleet, a quorum guard, an unobservable-cluster guard, and a give-up counter -
treated as the load-bearing part. Deliberately stubbed there: snapshot payloads sit in Postgres under
the env-derived AES key rather than object storage under a KMS one, and vSphere/Proxmox cannot report
VM power state (no API client - everything goes through OpenTofu), so those backends lose the cheapest
repair rung. When you take a shortcut, say so in the same style.
