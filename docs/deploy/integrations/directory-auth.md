# Directory authentication (Active Directory / LDAP)

By default KubeHarbor uses local accounts with open self-registration. Point it at an Active Directory
(or any LDAP v3 directory) and users sign in with their **organisation account** instead: accounts are
created here on first login, groups and roles come from configurable mapping rules, and self-registration
is disabled.

Two orthogonal switches, mirroring `KAAS_INFRA_PROVIDERS` vs `KAAS_PROVISIONER`:

| Variable | Meaning |
|---|---|
| `KAAS_AUTH=local\|ldap` | the *mechanism* - authenticate against a directory at all? |
| `KAAS_LDAP=fake\|real` | the *seam* - a real DC, or the in-memory fake |

So `KAAS_AUTH=ldap KAAS_LDAP=fake` runs the whole flow - group seeding, provisioning, membership sync -
with no domain controller, which is how you validate your rules before going live.

Only the **API** ever receives any of this. The worker doesn't authenticate users, so the bind password
stays out of the container holding the hypervisor keys and every tenant's secrets.

## 1. Write the mapping config

The rules are a list of raw LDAP filters, so they live in a **mounted YAML file** rather than env vars.
Copy the annotated example - it documents every field and the common AD topologies:

```bash
cp deploy/ldap.example.yaml deploy/ldap.yaml   # gitignored
```

The essentials:

- **Which DCs** - tried in order, so the second is a failover.
- **The service account** that searches (`bind_dn`), and where users live (`user_base_dn` - narrow it
  to an OU and only those people can log in at all).
- **The mapping rules.** Each rule's `filter` is a raw LDAP filter evaluated against the user's own
  entry, so anything the directory can express works - `memberOf`, AD's nested-group matching
  (`memberOf:1.2.840.113556.1.4.1941:=...`), or a plain attribute like `(department=Engineering)`.

Rules sharing a `group_key` become **one** portal group, which is how you say *"the whole team can see
these clusters, but only the admins can change them"*:

```yaml
- {group: Engineering, group_key: engineering, role: read,  filter: "(memberOf=CN=K8s-Eng,...)"}
- {group: Engineering, group_key: engineering, role: write, filter: "(memberOf=CN=K8s-Eng-Admins,...)"}
```

A user matching several rules for one group gets the highest role. Matching *no* rule is still a valid
account - they just have no group and own only their own clusters.

## 2. Try it with no domain controller

This validates your real rules and synthesizes one user per rule (`ad-<group_key>-<role>`, plus
`ad-everyone` and `ad-nobody`; password `demo`):

```bash
KAAS_AUTH=ldap KAAS_LDAP=fake make up-fake
```

Sign in as e.g. `ad-engineering-read` and check **Administration → Groups**: the mapped groups exist,
are badged *Directory*, and the account landed in the right one with the right role. Prove the config
here before pointing it at production.

## 3. Go live

In `.env`:

```bash
KAAS_AUTH=ldap
KAAS_LDAP_CONFIG_HOST=./ldap.yaml
KAAS_LDAP_BIND_PASSWORD=<the service account's password>
```

Then `make up`. At boot the API creates one group per rule; accounts appear as people sign in, at
**zero quota** until an admin grants some.

## What's load-bearing

- **`ldap://` is plaintext.** A simple bind sends the password in the clear, so the config refuses it
  unless you keep `start_tls: true` (the default) or explicitly set `insecure: true`. Internal AD certs
  usually need `ca_file` pointing at your internal CA's PEM.
- **The seeded admin is break-glass.** It logs in with its own password even when the DCs are
  unreachable - which is how you get in during a DC outage, and the account `make kubeconfig` and `make
  down` use. Its username is owned exclusively; the directory can never claim it, so set
  `KAAS_ADMIN_USERNAME` to a name no real person has.
- **Login is throttled in Postgres.** `/auth/login` is public, and with a directory every failed guess
  is a real bad-password bind against a real AD account. `KAAS_LDAP_MAX_FAILURES` (per username) must
  stay **below your AD lockout threshold** - the platform must never be what locks an account. The
  per-IP limit is higher because an office shares one NAT egress address.
- **Rules grant group roles only, never platform admin.** The admin flag is seed-only.
- **Directory groups are read-only in the portal.** To rename one, change `group:` in the config with
  `group_key` unchanged; to remove one, delete its rule - which leaves the group behind (marked *rule
  removed*), so a config typo can never destroy a team and everyone's membership of it.

## The gap: deprovisioning

Sessions are stateless and aren't re-checked against the directory, so disabling a user in AD leaves
their session valid until it expires (up to 24h), and their platform row (quota, cluster ownership)
outlives the directory account because nothing deletes it. Group changes land at the user's next login;
there's no background sync. This is a deliberate lab-scale shortcut - a production build would add
server-side revocation plus SCIM or a periodic sync.
