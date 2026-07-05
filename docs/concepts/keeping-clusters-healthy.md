# Keeping clusters healthy

Building a cluster is the easy half. The half that usually needs a Kubernetes expert - and that a
hand-rolled kubeadm cluster leaves entirely to you - is *keeping it healthy over months*. KubeHarbor
treats that as the platform's job. This page describes the five things it does automatically, all of
them modelled as ordinary phases of the same [reconciliation loop](architecture.md).

Every one of these can be tuned or turned off through the [configuration
reference](../deploy/configuration.md); all are on by default in real mode.

## Health checks

Independently of provisioning, the worker evaluates a set of **health checks** against every Ready
cluster on a short cadence, rolls them into a worst-of status, and stores one snapshot per cluster:

- API server reachable
- nodes Ready and not under pressure
- kube-system workloads healthy
- scheduling capacity (no stuck Pending pods, no cordoned nodes)
- etcd quorum and etcd backend-store headroom
- control-plane certificate expiry
- etcd backup freshness
- each installed add-on's workloads rolled out
- whether auto-repair has given up on a node

Health is an **observed axis orthogonal to the lifecycle phase** - a `Ready` cluster can report
`degraded`. The portal shows the per-check breakdown on the cluster [Overview
tab](../portal/managing-clusters.md) and a badge beside the phase. Health *observation* never changes a
cluster's phase on its own; what acts on a fault is automatic repair, below.

## Automatic repair

A Ready cluster whose node has gone NotReady, whose VM is powered off, or that never joined is detected
and repaired through a **ladder of increasingly expensive actions**, cheapest first:

| Fault | Repair |
|---|---|
| an add-on's workloads are unavailable | reinstall it |
| the VM is powered off | power it on *(libvirt only)* |
| the node never joined | rejoin it |
| the node is NotReady | restart containerd + kubelet |
| nothing cheaper worked | rebuild the node (keeping its data disks) |
| a sole control plane is unrecoverable | restore it from a stored snapshot |

The platform already knew how to do each of these - the value here is **deciding when to act, and
when not to.** The refusal conditions are the load-bearing part:

- **A cluster you cannot see is not a cluster that is broken.** If the API server is unreachable or the
  health snapshot is stale, every node reads NotReady, and the honest conclusion is "I can't see", not
  "rebuild everything" - so the platform stands down. The one exception is a VM the *hypervisor* itself
  reports as off, which is corroboration from below.
- **Blast-radius caps**, per cluster and per fleet: past a threshold of unhealthy nodes (or unhealthy
  clusters), this is one shared cause wearing many masks, and repair stops rather than amplifying it.
- **Quorum outranks the emergency**: never replace a control plane while another etcd member is
  unreachable.
- **Give up loudly**: after a bounded number of attempts a node is *suspended* and left alone, and the
  `auto-repair` health check turns unhealthy - a human is needed. A repair loop is worse than the
  fault.

Rebuilding a node re-creates its VM and root disk from the same golden image while **preserving its
extra data disks** (and their Longhorn replicas) on every provider.

## Periodic control-plane backups

Every Ready cluster is backed up on a cadence (6 hours by default): an **online** etcd snapshot of the
keyspace, plus the PKI (`/etc/kubernetes`) and kubelet state, sealed with the platform key and stored
in Postgres.

It exists for the one fault nothing else can reach: a **sole control plane whose VM is
unrecoverable.** Everywhere else the platform copies state off a live node or leans on a surviving
quorum member - both assume something is still running, which is exactly what fails when the only node
dies. The PKI rides along because a keyspace snapshot alone can't rebuild a control plane: without the
original CA key, every kubelet cert and per-user kubeconfig stops verifying.

A snapshot is the entire cluster's secrets in plaintext plus the CA private key, so it is sealed before
it reaches the store, exposed through **no API surface** (there is no download button), and verified on
capture - an unverifiable snapshot is never stored, because a corrupt backup is worse than none.

## Certificate rotation

kubeadm control-plane certificates expire about a year after issuance. A Ready, idle cluster is never
otherwise re-reconciled, so those certificates would silently lapse - and every worker-side feature
reads the stored admin kubeconfig, whose client cert lapses with them.

The platform observes each Ready cluster's earliest cert expiry and, once it's within the renewal
window (30 days by default), renews all control-plane certificates (`kubeadm certs renew all` plus a
static-pod restart so the API server reloads them), then re-seals the stored kubeconfig. HA clusters
renew one control plane at a time, keeping quorum. The CA (10-year) and kubelet certs (which
auto-rotate) are out of scope by design.

## etcd maintenance

etcd's on-disk store never shrinks on its own: compaction frees keyspace logically, but only
**defragmentation** returns the space to the filesystem. Left alone, the store grows to its high-water
mark, and on reaching its backend quota etcd arms a `NOSPACE` alarm and the whole cluster goes
**read-only**. The ordinary etcd-quorum health check can't see this coming - every member is up and in
quorum; they just refuse writes.

KubeHarbor handles this in two halves:

- **Prevention** (most of the value): every cluster's etcd is built with a raised backend quota (8 GiB)
  and etcd-side periodic auto-compaction as a backstop.
- **Remediation**: the platform observes each cluster's etcd on a cadence and, when it's fragmented
  past sensible thresholds (a ratio *and* an absolute size floor, so it doesn't take an outage to
  reclaim a few megabytes), defragments it **one member at a time**, moving leadership off each member
  first and disarming any `NOSPACE` alarm afterward. An armed alarm bypasses the maintenance window -
  that cluster is already read-only, so this is outage recovery, not hygiene.

Defragmentation briefly blocks the member it runs on, so it refuses to proceed while any member is
unreachable, and respects a configurable [maintenance window](../deploy/configuration.md).

## The common thread

None of these are cron jobs bolted onto the side. Each is desired state ("certs valid for at least the
window", "etcd not too fragmented", "a fresh backup exists") reconciled by the same loop that builds
clusters, with the same idempotency and durable retries - and each is fronted by guards that decide
whether acting is *safe*, not just whether it's *due*. That is what lets the platform tend a cluster
without becoming a hazard to it.
