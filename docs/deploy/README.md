# Operator guide

This section is for running KubeHarbor. It covers how to deploy it, which infrastructure it runs on,
and every knob you can turn.

## Pick a deployment path

| Path | Use it when | Guide |
|---|---|---|
| **Browser demo** | You want to look at it right now, with nothing installed | [Browser demo](browser-demo.md) |
| **Fake mode** | You want to see the platform, develop, or demo it - no hypervisor needed | [Fake mode](fake-mode.md) |
| **Podman Compose** | You're running on one machine (a homelab box, a WSL2 host) | [Compose](compose.md) |
| **Helm / Kubernetes** | You already have a cluster to host KubeHarbor on | [Helm](helm.md) · [Releasing](releasing.md) |

All three run the **same** platform - the same Go binaries, reconcile loop, and portal. They differ
only in how the containers are orchestrated and where the hypervisor is.

## Pick your infrastructure

A cluster is provisioned on **one infrastructure**, chosen at create time. A deployment can offer one
or several (`KAAS_INFRA_PROVIDERS`); when more than one is enabled, the create wizard gains an
**Infrastructure** step.

| Provider | Shape | Guide |
|---|---|---|
| **libvirt / KVM** | A dedicated, isolated network **per cluster**. Local hypervisor or remote over SSH. | [libvirt](providers/libvirt.md) |
| **VMware vSphere** | Clones a VM template onto the operator's shared portgroup. | [vSphere](providers/vsphere.md) |
| **Proxmox VE** | Clones a VM template onto the operator's shared bridge. | [Proxmox](providers/proxmox.md) |

Each provider clones **golden images** - VM images with kubeadm and containerd pre-baked, so cluster
creation is a fast clone rather than an apt run. Build them with [Packer](golden-images.md).

## Before real mode: what you need

1. A host with the hypervisor reachable (a WSL2 box with KVM, or L3 routing to vCenter / Proxmox).
2. **Golden images** built for your provider - see [Golden images](golden-images.md).
3. A `.env` with the real knobs - copy [`.env.example`](../../.env.example) and fill it in. Every
   value it documents is cross-referenced in the [configuration reference](configuration.md).
4. An SSH keypair the platform uses to reach the cluster VMs.

Then `make up` brings up the real stack. The full walkthrough is in [Compose](compose.md).

## Optional integrations

KubeHarbor works out of the box, and layers in these when you configure them:

- [**Directory authentication**](integrations/directory-auth.md) - sign in with Active Directory /
  LDAP accounts instead of local ones.
- [**Cluster DNS**](integrations/dns.md) - give every cluster a subdomain and publish its ingress
  address automatically.
- [**HashiCorp Vault**](integrations/vault.md) - a per-cluster secret store, consumed in-cluster by
  External Secrets and surfaced in the portal.
- [**NetBox IPAM**](integrations/netbox.md) - record the addresses shared-network clusters occupy.

## Keeping a deployment running

- **Two volumes hold everything that outlives a container**: `pgdata` (Postgres - the single source of
  truth) and `workdata` (each cluster's OpenTofu state, a derived cache). Everything else - the app
  binaries, the OpenTofu/Ansible/Helm toolchain, the provider plugins - is baked into images you can
  rebuild and recreate freely (`make rebuild`) without disturbing running clusters.
- **Never change `KAAS_SECRET_KEY` across an upgrade.** It derives the key that encrypts every stored
  kubeconfig and signs every session - rotate it and the stored secrets no longer decrypt. Keep it
  stable in `.env`.
- **DB migrations are forward-only** and run automatically at startup under an advisory lock.

## Versions, upgrades and rollbacks

KubeHarbor is published as five container images and a Helm chart, released by pushing a git tag. The
chart resolves its images from its own `appVersion`, so installing a version is all you do:

```bash
helm install kaas oci://ghcr.io/daniel-vaz/kaas-demo/charts/kaas --version 0.3.0
```

A running deployment names itself at `GET /api/version` and in the portal's sidebar footer. How to cut
a release, what to bump before tagging, how to pin or roll back, and how to verify a published image's
provenance are all in [**Releasing**](releasing.md).

## The full configuration surface

Every environment variable, grouped by area - platform, auth, providers, DNS, Vault, self-healing,
and the rest - is in the [**configuration reference**](configuration.md).
