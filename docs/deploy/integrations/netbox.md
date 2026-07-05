# NetBox IPAM

On the shared-network providers ([vSphere](../providers/vsphere.md) and
[Proxmox](../providers/proxmox.md)), many clusters share one operator-owned subnet. Setting
`KAAS_NETBOX_URL` makes the platform **record** every address its clusters occupy in NetBox - node IPs,
the HA VIP, and the default LoadBalancer IP - and release them when a cluster is destroyed.

It **records; it does not allocate.** The addresses are still decided by the site's DHCP server or by
the platform's own allocator, so cluster creation never depends on NetBox being up to make a decision.
KVM clusters are never registered - their networks are private and per-cluster, of no interest to a
site IPAM.

## Configuration

```bash
KAAS_NETBOX_URL=https://netbox.example.internal   # unset = the integration is not wired
KAAS_NETBOX_TOKEN=...                              # an API token...
# ...or mint one from a login:
# KAAS_NETBOX_USERNAME=...
# KAAS_NETBOX_PASSWORD=...
KAAS_NETBOX_INSECURE=0                             # 1 accepts a self-signed cert (lab)
KAAS_NETBOX_TAG=kaas                              # the tag our records carry
```

These are **worker-only** - the API doesn't provision, so it isn't given NetBox credentials.

## How it behaves

- **Coexists with an existing sync.** Records carry the `KAAS_NETBOX_TAG` plus a `kaas:<cluster-id>`
  description marker, so a deployment that already syncs vCenter into NetBox keeps working and the
  platform only ever deletes what it created.
- **Idempotent upserts** keyed on the address, so retries are safe.
- **A NetBox failure fails the reconcile step**, which then retries and converges. Silently skipping
  registration would let the IPAM drift - and on a shared subnet, drift is what hands one address to two
  machines, so failing loudly is the safe choice.
