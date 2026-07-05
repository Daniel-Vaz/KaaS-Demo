# Proxmox VE

Proxmox VE is architecturally the same kind of provider as [vSphere](vsphere.md): it clones a golden
**VM template** onto the operator's **shared bridge**, with `dhcp` or platform-allocated `static`
addressing, and the worker reaches the backend and the cluster VMs directly. The provider is OpenTofu
against the `bpg/proxmox` module (`infra/proxmox/`).

Enable it by adding `proxmox` to `KAAS_INFRA_PROVIDERS`. All `KAAS_PROXMOX_*` settings are
**worker-only**. Like vSphere it **cannot be combined with `KAAS_KVM_HOST`**, and the worker must have
L3 routing to the Proxmox subnet.

## Configuration

Authentication is **either an API token or a username/password - set exactly one**. A token is
preferred:

```bash
KAAS_PROXMOX_ENDPOINT=https://172.23.234.12:8006/
KAAS_PROXMOX_INSECURE=1                      # accept a self-signed cert (lab)
KAAS_PROXMOX_API_TOKEN=kaas@pve!tofu=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
# ...or:
# KAAS_PROXMOX_USERNAME=kaas@pve
# KAAS_PROXMOX_PASSWORD=...
KAAS_PROXMOX_NODE=proxmox01                  # the node VMs are created on
KAAS_PROXMOX_DATASTORE=Pool3ParNew           # datastore for VM disks + the cloud-init drive
```

Network - one operator bridge, two addressing modes (mirroring vSphere):

```bash
KAAS_PROXMOX_NET_BRIDGE=vmbr0
KAAS_PROXMOX_NET_VLAN=199                     # VLAN tag on a VLAN-aware bridge (omit/0 = untagged)
KAAS_PROXMOX_NET_MODE=dhcp                    # dhcp | static
KAAS_PROXMOX_NET_CIDR=172.23.234.0/24         # the bridge's subnet
# static only:
KAAS_PROXMOX_NET_GATEWAY=172.23.234.254
KAAS_PROXMOX_NET_DNS=172.23.234.10
KAAS_PROXMOX_NET_RANGE=172.23.234.50-172.23.234.70    # inclusive; free and outside any DHCP pool
```

Capacity ceiling (its own conserved pool):

```bash
KAAS_PROXMOX_BUDGET_VCPU=64
KAAS_PROXMOX_BUDGET_MEM_MB=131072
KAAS_PROXMOX_BUDGET_DISK_GB=4096
```

## Things specific to Proxmox

- **Addressing.** In `dhcp` mode the node's address is read back from the **QEMU guest agent** (so the
  golden image must ship `qemu-guest-agent` - the Packer build installs it). MACs are pinned in
  Proxmox's `BC:24:11` OUI so a re-created node reclaims its lease. In `static` mode the platform
  applies the allocated IP via Proxmox's native cloud-init `ip_config`. HA VIP and LoadBalancer IP are
  user-supplied in dhcp mode, allocated in static.
- **VLAN is a silent failure if wrong.** On a VLAN-aware bridge the node network usually lives on a
  tagged VLAN. Set `KAAS_PROXMOX_NET_VLAN` to match - get it wrong and the VM comes up with the right IP
  on the wrong L2 and is simply unreachable. If nodes never become reachable, check an existing VM's
  `net0` `tag=…` on the node.
- **Node names.** Proxmox VMs are named `<cluster-id>-<node>` for PVE-wide uniqueness, and its native
  cloud-init derives the guest hostname from that. The platform passes `--node-name` to `kubeadm` so the
  Kubernetes node name is always the bare platform-minted name regardless - invisible on other
  providers, load-bearing here.
- **Extra disks** live on a per-cluster **disk-owner VM** (a never-started VM that only holds the
  volumes, since `bpg/proxmox` has no standalone disk resource) and attach to nodes by
  `path_in_datastore` with a `serial` set to the platform-minted WWN. That keeps a node's data disks
  independent of its VM, so a rebuild preserves them. *(`path_in_datastore` is marked experimental
  upstream; the disk-preserving rebuild is validated by `tofu validate` and the fake, not yet on real
  hardware.)*

## Golden template

Proxmox golden images are VM **templates** named `<os>-k8s-<version>` on `KAAS_PROXMOX_NODE`, built with
`make golden-image-proxmox`. The build needs a seed template that carries `qemu-guest-agent` and a disk
grown past the stock image. Full steps in [Golden images § Proxmox](../golden-images.md#proxmox-ve).

## Optional: NetBox

As with vSphere, set `KAAS_NETBOX_URL` to record each cluster's addresses on the shared subnet - see
[NetBox](../integrations/netbox.md).
