# Cluster DNS

With cluster DNS configured, every cluster owns a **subdomain of a delegated zone** and the platform
publishes **one wildcard record** in it, pointing at the cluster's ingress address. That's the whole
user contract: deploy an app, attach an `HTTPRoute` for any name under the subdomain to the default
Gateway, and it resolves and routes - nothing else to configure.

```
*.apps.<cluster>.<base domain>.   A   <the cluster's default Envoy Gateway address>
```

Enable it by setting `KAAS_DNS_BASE_DOMAIN`. Point it at a zone **delegated to the platform** (e.g.
`kaas.example.internal`), not your AD domain itself - the service account will write everything in it.

## Configuration

`KAAS_DNS_BASE_DOMAIN` and `KAAS_DNS_APPS_LABEL` are read by **both** processes (the API derives each
cluster's domain names at admission and stores them on the row). Everything else - the server and the
credential that writes the zone - is the **worker's alone**, exactly like the hypervisor credentials.

```bash
KAAS_DNS_BASE_DOMAIN=kaas.example.internal   # unset = no cluster DNS at all
KAAS_DNS_APPS_LABEL=apps                      # apps land under *.apps.<cluster>.<base>
KAAS_DNS=nsupdate                             # worker only: fake | nsupdate | winrm
KAAS_DNS_SERVER=dc01.example.internal         # the DNS server (use its FQDN for GSS-TSIG)
KAAS_DNS_ZONE=kaas.example.internal           # the delegated zone
KAAS_DNS_AUTH=gss                             # gss (AD secure updates) | tsig | none
KAAS_DNS_KRB_USERNAME=svc-kaas@EXAMPLE.INTERNAL
KAAS_DNS_KRB_PASSWORD=...
```

For a shared-key zone use `KAAS_DNS_AUTH=tsig` with `KAAS_DNS_TSIG_KEYNAME` / `_SECRET` / `_ALGO`; for
a nonsecure zone, `KAAS_DNS_AUTH=none`. The worker image ships `nsupdate` and `kinit`, and discovers the
KDC from the AD domain's DNS SRV records, so you need no `krb5.conf` of your own.

## How the two halves split

- **The wildcard is the platform's.** It's published by the control plane at gateway-wiring time and
  **withdrawn before the cluster is destroyed** - because the cluster's ingress address is recycled to
  the next cluster on that subnet, and an orphaned wildcard would resolve into another tenant's gateway.
  Only the control plane knows a cluster's lifecycle, so only it writes this record.
- **The concrete names are external-dns's.** The bundled `external-dns` add-on lives in the cluster and
  publishes the specific hostnames a user's Services / Ingresses / HTTPRoutes ask for, confined to that
  cluster's own subdomain, against the same server and credential. It never touches the platform's
  wildcard.

## Windows DNS Server: use `winrm`

Windows DNS Server refuses to create **wildcard** records over RFC 2136 (`nsupdate`) at all -
`NOTAUTH`/`REFUSED` regardless of the zone's dynamic-update setting. Since the wildcard is the only
record the platform writes, `nsupdate` can't work against a Windows DC for it.

`KAAS_DNS=winrm` is the escape hatch: it drives the DNS Server role's PowerShell module over WinRM
instead, which has no such restriction. It reuses `KAAS_DNS_SERVER` / `_ZONE` / `_TTL` and adds its own
transport credential:

```bash
KAAS_DNS=winrm
KAAS_WINRM_HOST=dc01.example.internal
KAAS_WINRM_USERNAME=EXAMPLE\svc-kaas
KAAS_WINRM_PASSWORD=...
```

external-dns is untouched by this - it only ever creates concrete, non-wildcard names, so it keeps using
`nsupdate`/GSS-TSIG regardless of which registrar publishes the platform's own wildcard. (WinRM auth
defaults to NTLM against a default listener; a production build would use Kerberos.)

## Reachability

The wildcard points at an address on the **node network**, so a client has to route to it - true for
the shared-network providers (the operator's own subnet) and, on KVM, only from the hypervisor host or
wherever a route to the per-cluster bridge exists. That asymmetry is inherent to KVM's isolated
per-cluster networks, not something DNS fixes.

## Where it shows up

Once wired, the cluster's [Networking page](../../portal/networking.md) shows the wildcard record and
whether it's been published, alongside the reserved gateway address and every exposed application.
