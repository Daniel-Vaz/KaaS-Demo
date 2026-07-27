# libvirt / KVM

libvirt/KVM is the default infrastructure and the one the local Compose deployment targets. It's the
odd one out among the providers: **each cluster gets its own dedicated, isolated network**, so there's
nothing shared to configure. The provider is OpenTofu against the `dmacvicar/libvirt` module
(`infra/libvirt/`).

Enable it with `KAAS_INFRA_PROVIDERS=kvm` (the default) and `KAAS_PROVISIONER=tofu` on the worker.

## What it builds, per cluster

- **One dedicated NAT network** - a libvirt bridge unique to the cluster, addressed from a CIDR chosen
  at create time (auto-allocated from `KAAS_NET_SUPERNET`, or supplied by the user, and validated for
  overlap). NAT keeps clusters L2-isolated from each other while the host-networked worker can still
  reach the nodes and the nodes still have internet egress.
- **One VM per node** - a copy-on-write clone of the golden image, sized to the node's t-shirt size,
  with a cloud-init NoCloud disk that injects the SSH key and hostname and creates the `kaas` sudo user.
- **A pinned, deterministic MAC per node**, so a VM re-created during a rolling upgrade reclaims its
  DHCP lease and keeps its IP - which is what lets certificates and etcd membership (both keyed on the
  node IP) survive the rebuild.

The HA API VIP is derived deterministically as a high host in the cluster's own subnet - no pool to
configure, no cross-cluster collision possible.

## Networking

Three network domains coexist on the host (in WSL2, this is the Linux VM):

```
├─ Podman bridge (~10.89.x)          ← web, api, postgres, vault containers
├─ per-cluster libvirt NAT (virbrN)  ← one per cluster, own DHCP + gateway
│    ├─ <cluster>-cp-0   <cidr>.10
│    ├─ <cluster>-default-0  <cidr>.11
│    └─ (HA) API VIP     <cidr>.250   (floated by keepalived)
└─ host namespace                    ← routes to every NAT bridge; owns the libvirt socket
```

The **worker** is the only container that touches this. It runs `--network host` so it shares the host
namespace, which already routes to every cluster bridge, and it mounts the libvirt socket
(`/run/libvirt`). That keeps the KVM blast radius to exactly one container. Because it's host-networked,
it reaches Postgres over the published host port (`:5432`).

Your `kubectl` reaches a cluster directly from inside WSL2 (which routes to the bridges). From the
Windows host, `192.168.122.x`-style addresses aren't reachable by default - enable WSL2 mirrored
networking (`networkingMode=mirrored` in `.wslconfig`) or run `kubectl` inside WSL2.

## Preconditions (local hypervisor)

- Nested virtualization enabled so KVM works in WSL2.
- `libvirtd` running; `/run/libvirt/libvirt-sock` present and accessible.
- The libvirt `default` storage pool exists and is started (a package install usually auto-creates it).
- A golden image built and reachable - see [Golden images](../golden-images.md).
- Host capacity budgeted (`KAAS_BUDGET_*`) - VMs oversubscribe RAM fast; an HA cluster is 3 control
  planes plus workers.
- `KAAS_NET_SUPERNET` / `KAAS_NET_RESERVED` don't clash with your host/Podman networks.

## Extra node disks

Because the `dmacvicar/libvirt` provider treats a domain's disk list as replace-on-change, KubeHarbor
declares extra disks as independent `libvirt_volume`s and attaches them to running nodes with `virsh`
rather than OpenTofu. This is invisible above the provision seam; the practical upshot is that a node's
extra data disks live in resources independent of its VM, so a node rebuild (a repair or OS roll)
preserves them. See [managing a cluster § node disks](../../portal/managing-clusters.md).

## Remote KVM hosts

The hypervisor is local **by default, not by design**. Set `KAAS_KVM_HOST` (plus an SSH user and key)
and the platform provisions onto another machine's libvirt, with every hop routed through the KVM host -
which by definition can reach both libvirtd and every cluster bridge, because it creates them:

```
worker (host-networked)                          KVM host                     cluster VMs
  ├── OpenTofu ──── qemu+ssh://user@host/system ──→ libvirtd ─────────────→ create/destroy
  ├── Ansible ───── ssh -W (ProxyCommand) ────────→ sshd ─── ssh ─────────→ :22 on each node
  └── kubectl/helm ─ ssh -D (SOCKS5) ─────────────→ sshd ─── TCP ─────────→ :6443 (API / VIP)
```

The kubectl path is the interesting one: rewriting the cluster's private address would be wrong (the
cert is issued for it, and the HA VIP only exists inside that subnet), so instead the worker holds a
SOCKS tunnel open and stamps `proxy-url` onto the platform-side copies of the kubeconfig. Every
kubectl/helm consumer - add-ons, metrics, health, workloads, monitoring, the terminal - routes through
it with no change. The shell sandbox shares that tunnel's *address* and nothing else, so it stays
credential-free.

Two things worth knowing for a remote host:

- **Golden images are staged, not imported.** Importing an image through the provider streams it over
  the libvirt connection once per cluster. So with `KAAS_KVM_HOST` the provisioner instead uploads each
  image into the hypervisor's pool once (over SSH) and every cluster's nodes back onto it there - under
  your own `KAAS_RECONCILE_JOB_TIMEOUT` budget, which you'll almost always need to raise. Measure your
  link and size the timeout above `image_size / rate`.
- **A bare KVM host needs a `default` storage pool** (`virsh pool-define-as default dir --target
  /var/lib/libvirt/images` + autostart + start), and host keys aren't verified unless
  `KAAS_KVM_KNOWN_HOSTS_FILE` is set.

A tenant's downloaded kubeconfig still points at the cluster's private subnet, so from a laptop they
need their own tunnel (`ssh -D 1080 <kvm-host>`, then `kubectl --proxy-url socks5://127.0.0.1:1080`).

## The module and its version pin

`infra/libvirt/versions.tf` allows `dmacvicar/libvirt ~> 0.9.8`, the line `main.tf` is written against.
0.9 was a ground-up rewrite of the provider - HCL maps ~1:1 onto libvirt's own domain/network/volume
XML - and shares no schema with 0.8, so the module spells out what 0.8 used to infer: the network's
gateway address and DHCP range, each disk's target device and bus, the cloud-init ISO as a pool volume
attached as a CD-ROM. Later 0.9.x bumps are safe to take; a 0.10 would want reading first. Validate the
module without touching libvirt:

```bash
cd infra/libvirt && tofu init -backend=false && tofu validate
```
