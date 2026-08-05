<div align="center">
<img src="assets/kaas-logo.png" alt="KubeHarbor" width="120" />

# KubeHarbor Documentation
</div>

Everything here describes the platform **as it is today**. The docs are split by who you are:

## 🧭 Concepts - understand the platform

Start here whether you'll operate it or use it.

- [**Why KubeHarbor**](concepts/why-kubeharbor.md) - the idea behind it: what a KaaS platform is for,
  and the design principles anyone building one should weigh.
- [**Architecture**](concepts/architecture.md) - how it's built: the reconciliation loop, the cluster
  state machine, the components, and the fake/real seams.
- [**Keeping clusters healthy**](concepts/keeping-clusters-healthy.md) - the self-healing story:
  health checks, automatic repair, backups, certificate rotation, and etcd maintenance.

## 🛠️ Operators - deploy and configure

Running KubeHarbor for others.

- [**Operator guide**](deploy/README.md) - the overview and the map of deployment paths.
- [**Fake mode**](deploy/fake-mode.md) - run the whole platform with no hypervisor.
- [**Browser demo**](deploy/browser-demo.md) - the whole control plane as WebAssembly, published as a
  static site with no backend at all.
- [**Podman Compose**](deploy/compose.md) - the container deployment (local, real, and scaled).
- [**Helm / Kubernetes**](deploy/helm.md) - deploy KubeHarbor onto an existing cluster.
- [**Configuration reference**](deploy/configuration.md) - every environment variable, by area.
- [**Golden images**](deploy/golden-images.md) - building the VM images with Packer.
- [**Releasing**](deploy/releasing.md) - the tag-driven release workflow, and how to install a
  published version.

**Infrastructure providers**

- [**libvirt / KVM**](deploy/providers/libvirt.md) - the local (and remote) hypervisor path.
- [**VMware vSphere**](deploy/providers/vsphere.md)
- [**Proxmox VE**](deploy/providers/proxmox.md)

**Optional integrations**

- [**Directory authentication**](deploy/integrations/directory-auth.md) - Active Directory / LDAP.
- [**Cluster DNS**](deploy/integrations/dns.md) - delegated zones and per-cluster records.
- [**HashiCorp Vault**](deploy/integrations/vault.md) - the platform secret store.
- [**NetBox IPAM**](deploy/integrations/netbox.md) - recording addresses on shared networks.

## 👤 Users - use the portal

Managing clusters through the web portal.

- [**Portal tour**](portal/README.md) - the layout and what each area does.
- [**Creating clusters**](portal/creating-clusters.md) - the create wizard, sizing, and options.
- [**Managing a cluster**](portal/managing-clusters.md) - the cluster detail tabs: nodes, disks,
  terminal, SSH, add-ons, activity, audit, upgrades.
- [**Workloads**](portal/workloads.md) · [**Storage**](portal/storage.md) ·
  [**Networking**](portal/networking.md) - inspecting what runs inside a cluster.
- [**Monitoring**](portal/monitoring.md) · [**Security**](portal/security.md) - the observability and
  posture dashboards.
- [**Secrets**](portal/secrets.md) - the Vault-backed per-cluster secret store.
- [**Catalog & custom catalogs**](portal/catalog.md) - the add-on library and your own charts.
- [**Account, teams & administration**](portal/account-and-admin.md) - profile, groups, quota, and the
  admin pages.

---

Looking for the deep rationale behind a specific decision? That lives in
[`CLAUDE.md`](../CLAUDE.md) and in the comments next to the code - this documentation stays focused on
what the platform does and how to run and use it.
