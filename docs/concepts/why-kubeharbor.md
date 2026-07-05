# Why KubeHarbor exists

KubeHarbor is a distillation of what a **Kubernetes-as-a-Service** platform can be. It grew out of
years of running Kubernetes and watching where the experience gets rough - and it is an opinionated
answer to a single question:

> How do you give someone a **full, real Kubernetes cluster they completely control**, without also
> handing them every operational burden that usually requires a Kubernetes expert to carry?

This page is about that idea and the trade-offs behind it. If you want to know how the platform is
built, read [Architecture](architecture.md); if you want to run it, start with the [operator
guide](../deploy/README.md).

## The gap it fills

There are two familiar shapes of "running Kubernetes", and both leave something on the table:

- **Managed Kubernetes from a public cloud** (EKS, GKE, AKS) is wonderfully easy to *start* - but the
  control plane is a black box you don't own, you're on someone else's bill and network, and "full
  control" stops at the API server. You cannot get a shell on a control-plane node or reason about
  etcd, because they aren't yours.
- **Rolling your own with kubeadm** gives you total control - and total responsibility. You own the
  VMs, the CNI, storage, ingress, certificates, etcd, backups, upgrades. Every one of those is a
  place a cluster quietly rots if you don't know to tend it, and knowing to tend it is exactly the
  expertise most teams are trying not to need.

KubeHarbor sits deliberately between them. **The cluster is entirely yours** - real nodes you can SSH
into, an etcd you can inspect, `cluster-admin` if you want it. But the platform **does the operational
tending for you**: it builds the cluster correctly, ships the things a production cluster needs turned
on by default, and keeps it healthy on your behalf.

Unlike the public clouds, **KubeHarbor does not hide or manage the control plane away from you** - it
guides you to a healthy one. A hand-rolled kubeadm deployment demands you already know how to keep etcd
defragmented, rotate certificates before they lapse, back up the control plane, and roll an OS upgrade
without losing quorum. Here, the platform knows how, and does it - while leaving the cluster open for
you to work in.

## The principles

Everything in the platform follows from a few beliefs.

### 1. The easiest possible end-user experience

A user should be able to think in terms of *"I want a cluster"* and *"I want it to keep working"* -
not in terms of golden images, cloud-init, VIPs, or Helm ordering. So the portal asks for a name, a
size, and a few choices, and the platform derives everything else. A cluster comes up with a CNI,
default storage, ingress, TLS, DNS, and audit logging **already working**, because a cluster without
those isn't really ready to use - it's a starting point that leaves the hard 20% as homework.

### 2. But a full cluster, fully yours

Ease of use must not come at the cost of control. So KubeHarbor never fences the user out of their own
cluster: the portal hands you a real kubeconfig, an in-browser `kubectl` shell, SSH onto the nodes,
and full workload management. The abstractions are *conveniences you can ignore*, not *walls you can't
see past*. If you want to do something the portal doesn't offer, you can - it's your cluster.

### 3. A healthy cluster is the platform's job, not the tenant's

This is the heart of it. The operations that keep a self-managed cluster alive - and that a newcomer
doesn't even know to worry about - are the platform's responsibility:

- **Backups** so a lost control plane comes back.
- **Certificate rotation** so a year-old idle cluster doesn't silently expire.
- **etcd maintenance** so a cluster doesn't wedge itself read-only.
- **Automatic repair** so a broken node is noticed and fixed without anyone asking.

The platform doesn't just detect problems - it *acts*, carefully. See [Keeping clusters
healthy](keeping-clusters-healthy.md).

### 4. A control plane, not a script

A "web form that runs a provisioning script" fails the moment anything goes wrong halfway through.
KubeHarbor is instead built the way Kubernetes itself is: **desired state in a database, and a
reconciliation loop that continuously drives reality toward it.** You ask for a cluster; the platform
records that wish and works toward it, one idempotent step at a time, retrying on failure and healing
drift. That's what makes "keep it healthy" a natural extension of "build it" rather than a bolted-on
afterthought. See [Architecture](architecture.md).

## Considerations for anyone building a KaaS platform

If you're thinking about building something like this, these are the decisions that matter most -
KubeHarbor is one set of answers to them, and the rest of these docs show the answers in practice.

- **Model desired state and reconcile toward it.** Do not orchestrate with a linear script. Every
  provisioning step must be idempotent and safe to retry, because everything eventually fails halfway.

- **Decide what "ready" means, generously.** A bare cluster is not a useful deliverable. Choosing the
  default add-ons - CNI, storage, ingress, DNS, TLS - *is* the product. KubeHarbor ships all of these
  on by default and wires them together (see [the default cluster](../portal/creating-clusters.md)).

- **Separate the tenant experience from the operational truth.** The user thinks in clusters and
  sizes; the platform thinks in VMs, IPs, VIPs, and quorum. Keep the mapping in one place so the loop
  stays simple and the portal stays friendly.

- **The refusal conditions are the feature.** An automatic repairer that always acts is a cluster
  shredder; a backup job that runs the wrong way stops every healthy cluster. The valuable, hard part
  of automation is knowing precisely *when not to act* - during a network partition, when quorum is at
  risk, when the whole fleet looks broken at once. KubeHarbor treats those guards as load-bearing.

- **Meter capacity honestly.** VMs oversubscribe a host fast. Capacity is a first-class admission
  check, per-infrastructure, so the platform can't promise a cluster it can't physically host.

- **Keep the secrets where they belong.** The component that talks to the hypervisor holds the keys;
  the component a user gets a shell in holds none. Blast radius is a design input, not an afterthought.

- **Make it demonstrable without the hardware.** Every backend has a fake behind the same interface,
  so the whole platform - portal, reconcile loop, state machine - runs and is testable with no
  hypervisor. That's not just for demos; it's what keeps the control-plane logic honest and fast to
  develop.

## What KubeHarbor is (and isn't)

KubeHarbor is a **functional, lab-scale platform** used for real at small scale - homelabs, test
environments, and small teams. It is deliberately **not** a hyperscale product: it runs a single
Postgres, leans on lab-grade shortcuts in a few places (env-derived keys, self-signed TLS on
localhost), and each of those is marked in the code with a note on what a production build would do
instead. Those shortcuts are peripheral; the load-bearing control-plane patterns - reconciliation,
the state machine, idempotency, durable retries, HA control planes, self-healing, multi-tenancy - are
built for real.

That's the point: KubeHarbor is where the *ideas* of a KaaS platform are worked out at a size one
person can hold in their head, run on a laptop, and actually use.
