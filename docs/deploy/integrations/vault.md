# HashiCorp Vault

KubeHarbor integrates a central **HashiCorp Vault** as the platform's secret store. There's always
**one** Vault, deployed next to the platform - "per-cluster" means a KV *path* and a set of policies,
never a Vault per cluster. Each cluster gets its own subtree and access bindings that **mirror the
portal's own read/write model**, so "only people with access to a cluster can touch its secrets, writers
can edit, readers can only view" is true in Vault itself, not just in the portal.

Inside each cluster, the bundled **External Secrets Operator** reads that cluster's subtree and
materialises Kubernetes Secrets from it. The portal's [Secrets page](../../portal/secrets.md) surfaces
all of this and hands users a deep-link into the Vault UI.

## The KV layout

Everything the platform writes lives under one KV v2 mount (default `kaas`), so a Vault you already run
for other things keeps its own mounts untouched:

```
kaas/platform/*                        platform-owned secrets (admins only)
kaas/clusters/<cluster-id>/<ns>/<n>     per-cluster, per-namespace tenant secrets
```

## Configuration

The Compose stack ships a **single-node Vault container** on file storage (the `vaultdata` volume),
configured by `deploy/vault/vault.hcl` and started by `deploy/vault/entrypoint.sh`, which initialises
it with one unseal share on first boot, unseals it on every boot, and mints a token whose id is the
fixed `KAAS_VAULT_TOKEN` - so the whole flow works out of the box with no manual unseal step. The
unseal key and initial root token are written to `init.json` inside that volume: a deliberate lab
shortcut (production would use KMS auto-unseal and never materialise an unseal key), but the state is
durable across restarts, which matters - see [Recovering lost Vault state](#recovering-lost-vault-state).

The relevant settings:

```bash
KAAS_VAULT=real                              # fake = in-memory (default in fake mode)
KAAS_VAULT_ADDR=http://vault:8200            # the platform's own route to Vault (API + worker)
KAAS_VAULT_CLUSTER_ADDR=                     # the route from inside a cluster (default: = _ADDR)
KAAS_VAULT_TOKEN=kaas-root-dev               # the management/minter token
KAAS_VAULT_MOUNT=kaas                        # the KV v2 mount the platform owns
KAAS_VAULT_UI_URL=http://localhost:8200      # browser-facing base for "View in Vault"
```

The token is split by role, the same way the reconcile loop splits per-cluster from singleton work:

- The **worker** holds the broad **management token** and provisions the mount, policies, identity, and
  per-cluster paths (and keeps the access bindings converged with Postgres under the leader lease).
- The **API** holds a narrow **minter token** used only to mint the short-lived token behind the "View
  in Vault" handoff.

> **For a real cluster**, set `KAAS_VAULT_CLUSTER_ADDR`: it is the address written into each cluster's
> `ClusterSecretStore`, so the in-cluster External Secrets Operator must be able to reach it from inside
> the cluster - a node-network address (e.g. `http://<host-ip>:8200`), not `host.containers.internal`.
> Leave `KAAS_VAULT_ADDR` on the platform's own local route. Pointing *it* at the tenant-facing address
> instead puts the reconcile loop on that route, and when the route breaks every cluster loops in
> `InstallingAddons` re-running its add-on installs, rather than just ESO failing to sync.

## Auth mode follows the portal

The Vault auth backend the platform configures follows `KAAS_AUTH`:

- `local` → Vault **userpass**.
- `ldap` → Vault **ldap**, configured from the same directory settings the portal uses (translated from
  your [directory-auth config](directory-auth.md)), so Vault authenticates users against the same
  directory with the same filter.

## Lifecycle

- **Per-cluster provisioning** (create the subtree and policies) is driven by the reconcile loop and
  released **before** the cluster's infrastructure is destroyed - the same shape as [DNS](dns.md).
- **Access convergence** (keeping Vault's policies and identity groups in sync with the portal's
  ownership and group memberships) runs under the leader lease on a ticker, because membership edits
  happen API-side and don't bump a cluster's generation.

The fake records state in memory and logs, so admission, wiring, the Secrets page, and the handoff are
all demoable under `make up-fake` with no Vault at all.

## Recovering lost Vault state

Vault's state must outlive its container, because `VaultWired` **latches**: once a cluster is wired,
nothing clears the marker, so the reconcile loop never re-provisions its path. `EnsurePlatform` (the
mount, the admin policy, the auth backend) is likewise logged-not-fatal and runs only at leader
startup. So if Vault loses its data - the `vaultdata` volume removed, an external Vault rebuilt, or a
Helm deployment left on the subchart's in-memory dev mode - restarting things is not enough.

The symptom is the **Secrets page's "View in Vault" button**: Vault will happily issue a token naming a
policy it does not have, and the Vault UI then rejects it with

```
preflight capability check returned 403, please ensure client's policies grant access to path "kaas/"
```

The API now refuses to mint such a token and returns that reason instead, so the failure surfaces in
the portal rather than one UI away. To recover:

```bash
podman restart kaas-worker    # re-runs EnsurePlatform: the mount, kaas-admin, the auth backend
psql "$DATABASE_URL" -c "update clusters set vault_wired=false where phase='ready';"
```

Clearing the marker makes the next reconcile tick re-run `EnsureCluster` per cluster, rewriting its
policies, KV subtree and ESO auth role. It is idempotent and touches no secret **data** - only
`releaseVault`, on delete, ever removes that. Secrets written to a Vault whose storage was lost are
gone with it; this restores the paths and access, not their contents.

## Deploying real Vault

The Compose Vault is not production-grade. For a real deployment, run HA Vault with auto-unseal via
a KMS, persistent storage, and an audit device, and point `KAAS_VAULT_ADDR` / `KAAS_VAULT_TOKEN` at it.
The platform only ever writes under its own `kaas` mount and manages its own policies/identity, so it
coexists with an existing Vault.
