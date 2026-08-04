<div align="center">

<img src="docs/assets/kaas-logo.png" alt="KubeHarbor" width="160" />

# KubeHarbor

### Kubernetes Without the Rough Seas

[![CI](https://github.com/Daniel-Vaz/KaaS-Demo/actions/workflows/ci.yml/badge.svg)](https://github.com/Daniel-Vaz/KaaS-Demo/actions/workflows/ci.yml)
[![Release](https://github.com/Daniel-Vaz/KaaS-Demo/actions/workflows/release.yml/badge.svg)](https://github.com/Daniel-Vaz/KaaS-Demo/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/Daniel-Vaz/KaaS-Demo?sort=semver&label=release)](https://github.com/Daniel-Vaz/KaaS-Demo/releases)

A self-hosted **Kubernetes-as-a-Service** platform. Request a cluster from a web portal and
KubeHarbor builds it end to end - virtual machines, a `kubeadm` control plane, a CNI, storage,
ingress, DNS, monitoring and more - then keeps it healthy for you. You get a full cluster you're
in complete control of; the platform handles the parts that usually take a Kubernetes expert.

</div>

---

KubeHarbor provisions VMs with **OpenTofu**, forms them into clusters with **Ansible + kubeadm**,
installs add-ons with **Helm**, and holds everything to its desired state with a **level-triggered
reconciliation loop** backed by **Postgres** - the same control-plane shape a cloud provider's
managed Kubernetes uses, sized for a homelab or a small team.

It runs on **libvirt/KVM**, **VMware vSphere**, or **Proxmox VE**, as **Podman containers** or a
**Helm release** on an existing cluster - and there's a **fake mode** that runs the whole portal and
control plane with no hypervisor at all, for demos and development.

>[!TIP]
> **New to KubeHarbor?** The fastest way to see it is fake mode:
> ```bash
> make up-fake      # portal at http://localhost:8080 - sign in as admin / admin
> ```
> Already have a Kubernetes cluster? Install a published release onto it:
> ```bash
> helm install kaas oci://ghcr.io/daniel-vaz/kaas-demo/charts/kaas --set providers=fake
> ```

## Documentation

| I want to… | Start here |
|---|---|
| **Understand what KubeHarbor is and why it exists** | [Why KubeHarbor](docs/concepts/why-kubeharbor.md) · [Architecture](docs/concepts/architecture.md) |
| **Deploy and configure the platform** | [Operator guide](docs/deploy/README.md) |
| **Use the web portal** | [Portal user guide](docs/portal/README.md) |
| **Install, pin or cut a release** | [Releasing](docs/deploy/releasing.md) |
| **Browse everything** | [Documentation index](docs/README.md) |

## What you get

- **A cluster in a few clicks** - a create wizard picks a size, a version bundle, add-ons and
  storage; the platform provisions the VMs and forms the cluster. Single-node or 3-node HA control
  planes.
- **Batteries included** - every cluster ships with a CNI (Cilium), persistent storage (Longhorn),
  north-south ingress (MetalLB + Envoy Gateway), TLS (cert-manager), DNS, and audit logging on by
  default.
- **A full console** - browse and scale workloads, inspect storage and networking, read monitoring
  and security dashboards, open an in-browser `kubectl` shell or SSH into a node, and manage secrets
  in Vault - all from the portal, [pictured throughout the user guide](docs/portal/README.md).
- **Self-healing** - clusters are backed up, their certificates and etcd are maintained, and broken
  nodes are detected and repaired automatically. See [Keeping clusters
  healthy](docs/concepts/keeping-clusters-healthy.md).
- **Multi-tenant** - local or Active Directory / LDAP accounts, per-user per-infrastructure quota,
  and teams that share clusters under a read/write role.

## Quick reference

```bash
make up-fake     # fake mode: portal + api + postgres, no hypervisor (great for a first look)
make up          # real mode: + a host-networked worker driving real VMs on libvirt/KVM
make up-scale    # scaled real mode: N replicas of each tier behind a load balancer
make down        # delete running clusters, then stop the stack
make help        # every target
```

Releases are cut by pushing a tag - `v1.4.0` for the platform's five images, `chart-v0.3.0` for the
Helm chart. `make release-check` verifies the version is consistent before you do; the whole workflow
is in [Releasing](docs/deploy/releasing.md).

The portal is at **http://localhost:8080**; the JSON + SSE API is on **http://localhost:8081**.
Real mode needs a `.env` - see [`.env.example`](.env.example) and the [operator
guide](docs/deploy/README.md).

## Repository layout

```
cmd/            API, worker, and the isolated shell / node-ssh agent binaries
internal/       the platform: domain model, reconcile loop, and every backend seam
infra/          OpenTofu modules (libvirt, vsphere, proxmox)
ansible/        playbooks + roles (kubeadm bring-up, CNI, HA, upgrades, maintenance)
packer/         golden-image builds (kubeadm/containerd pre-baked)
web/portal/     the React + TypeScript + Mantine portal SPA
deploy/         Containerfiles, Podman Compose files, and the Helm chart
migrations/     Postgres schema
docs/           this documentation
```

Detailed design notes and the rationale behind the load-bearing decisions live in
[`CLAUDE.md`](CLAUDE.md) and alongside the code they describe.
