# VMware vSphere

The vSphere provider clones a golden **VM template** onto the operator's **shared portgroup**. Unlike
libvirt, there's no per-cluster network - every cluster's VMs sit on one operator-owned network, so the
network is deployment configuration, not a per-cluster choice. The provider is OpenTofu against the
`hashicorp/vsphere` module (`infra/vsphere/`).

Enable it by adding `vsphere` to `KAAS_INFRA_PROVIDERS`. All `KAAS_VSPHERE_*` settings are **worker-only**
- the API is given only the network shape and budget it needs for admission, never vCenter credentials.

## Reachability is a precondition

The worker reaches vCenter (OpenTofu), the cluster VMs (Ansible/SSH), and the API server
(kubectl/Helm) **directly** - there's no tunnel or bastion. So the host running the worker must have L3
routing to the vSphere network. Consequently vSphere **cannot be combined with `KAAS_KVM_HOST`** (whose
SSH/SOCKS rerouting is global and would misroute vSphere traffic); the app refuses to start if both are
set.

## Configuration

Placement and credentials:

```bash
KAAS_VSPHERE_URL=https://vcenter.example.internal
KAAS_VSPHERE_USERNAME=DOMAIN\user
KAAS_VSPHERE_PASSWORD=...
KAAS_VSPHERE_INSECURE=1                     # accept a self-signed vCenter cert (lab)
KAAS_VSPHERE_DATACENTER=MyDC
KAAS_VSPHERE_CLUSTER=CLUSTER01                     # compute cluster
KAAS_VSPHERE_DATASTORE=datastorefc10k    # SEE the latency note below
KAAS_VSPHERE_FOLDER=DVaz                      # holds the templates + one subfolder per cluster
```

Network - one operator portgroup, two addressing modes:

```bash
KAAS_VSPHERE_NETWORK=serviceVMNetwork
KAAS_VSPHERE_NET_MODE=dhcp                   # dhcp | static
KAAS_VSPHERE_NET_CIDR=172.23.252.0/24        # the portgroup's subnet
# static only:
KAAS_VSPHERE_NET_GATEWAY=172.23.252.1
KAAS_VSPHERE_NET_DNS=172.23.252.10,172.23.252.11
KAAS_VSPHERE_NET_RANGE=172.23.252.200-172.23.252.239   # inclusive; free and outside any DHCP pool
```

Capacity ceiling (its own conserved pool):

```bash
KAAS_VSPHERE_BUDGET_VCPU=64
KAAS_VSPHERE_BUDGET_MEM_MB=131072
KAAS_VSPHERE_BUDGET_DISK_GB=4096
```

## Addressing

- **`dhcp`** - the portgroup's DHCP server assigns node IPs; the platform reads them back via
  open-vm-tools. Node MACs are pinned (deterministic, in VMware's `00:50:56:00–3f` static range), so a
  re-created VM reclaims its lease - the stable-IP contract rolling upgrades depend on. This needs your
  DHCP leases long enough (or MAC-keyed reservations) that a node's brief absence during a rebuild
  doesn't lose the address; if that's not true, use `static`.
- **`static`** - the platform allocates node IPs, the HA VIP, and the LoadBalancer IP from
  `KAAS_VSPHERE_NET_RANGE` and injects them via cloud-init guestinfo, persisting them per node so a
  rebuild keeps the address.

For an **HA** cluster in `dhcp` mode, the **user supplies the API VIP** at create time (only they know
what's free outside the DHCP pool). The **LoadBalancer IP** (for the default MetalLB/Envoy Gateway) is
also user-supplied in dhcp mode - and required on *every* dhcp cluster, since ingress ships by default.

## Datastore choice matters most

`KAAS_VSPHERE_DATASTORE` is a **latency** decision, not a capacity one, and it's the single
infrastructure choice most able to take a cluster down.

Every control-plane node runs etcd, which fsyncs its write-ahead log **on the critical path of every
write** and expects that to land in single-digit milliseconds. Building a cluster clones and boots all
its VMs at once and pulls the CNI image onto every node concurrently - precisely a datastore I/O storm,
and the control plane breaks first. On nearline 7.2K spindles shared with a busy lab, a single etcd
`fdatasync` has been observed to stretch to over two minutes, costing quorum through dozens of leader
changes. **Use flash or 10K+ FC storage** for the control planes.

## Golden template

vSphere golden images are vCenter VM **templates** named `<os>-k8s-<version>`, built with `make
golden-image-vsphere`. The build needs a seed template imported from the Ubuntu **OVA** (not the `.img`
- the OVA ships open-vm-tools), and the platform strips the OVA's inherited vApp config with
`cmd/vsphere-prep` so the provider will clone it and cloud-init picks the right datasource. Full steps
in [Golden images § vSphere](../golden-images.md#vmware-vsphere).

## Optional: NetBox

On a shared network, recording addresses in an IPAM keeps it from drifting into handing one address to
two machines. Set `KAAS_NETBOX_URL` to have the platform record each cluster's node IPs, VIP, and
LoadBalancer IP - see [NetBox](../integrations/netbox.md).
