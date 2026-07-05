# Infrastructure - the OpenTofu modules

Creates and destroys the VMs backing a cluster with **OpenTofu**. One module per infrastructure
provider; a thin Go wrapper renders the module into a per-cluster workspace, runs `tofu init/apply`,
and reads node IPs back from `tofu output`. Shared mechanics live in `internal/provision/tofurunner`.

```
infra/
  libvirt/    dmacvicar/libvirt  - a dedicated NAT network + COW volumes + domains, PER cluster
  vsphere/    hashicorp/vsphere  - clone a VM template onto the operator's shared portgroup
  proxmox/    bpg/proxmox        - clone a VM template onto the operator's shared bridge
```

Each module emits the same `nodes` output (`{name, ip, mac, disks}`), so one parser serves all three
and nothing above the provision seam is provider-aware.

## Provider version pins

- `libvirt/versions.tf` pins **`dmacvicar/libvirt ~> 0.8.0`** - the classic schema `main.tf` uses. The
  0.9.x line is an incompatible ground-up rewrite; **do not bump the pin without rewriting `main.tf`.**
- `vsphere` pins `hashicorp/vsphere`, `proxmox` pins `bpg/proxmox`.

Validate any module locally without touching a backend:

```bash
cd infra/libvirt && tofu init -backend=false && tofu validate
```

## More

- Per-provider behaviour, networking, and gotchas: the provider guides -
  [libvirt](../docs/deploy/providers/libvirt.md), [vSphere](../docs/deploy/providers/vsphere.md),
  [Proxmox](../docs/deploy/providers/proxmox.md).
- The golden images these modules clone: [golden images](../docs/deploy/golden-images.md).
- How the pieces fit into the control plane: [Architecture](../docs/concepts/architecture.md).
