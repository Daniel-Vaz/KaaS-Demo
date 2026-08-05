# Security Policy

KubeHarbor provisions real virtual machines, holds every tenant's cluster credentials, and - in a
real deployment - carries a fleet-wide SSH key and a Vault management token. Security reports are
genuinely welcome.

Please read [Scope](#scope) before reporting. A specific set of weaknesses in this project are
**deliberate, documented lab-scale shortcuts** rather than defects, and knowing which is which will
save us both time.

## Supported versions

KubeHarbor is pre-1.0 and released from a single line. There are no maintenance branches and no
backports: fixes land on `main` and go out in the next release.

| Version | Supported |
|---|---|
| Latest release ([0.1.x](https://github.com/Daniel-Vaz/KaaS-Demo/releases/latest)) | ✅ |
| `main` | ✅ |
| Anything older | ❌ - upgrade first, then report if it persists |

Container images are published per release to GHCR and carry a keyless
[build provenance attestation](https://github.com/Daniel-Vaz/KaaS-Demo/attestations); the Helm chart
is versioned separately (`chart-v*`) and resolves image tags from its `appVersion`. See
[Releasing](docs/deploy/releasing.md).

## Reporting a vulnerability

**Report privately through GitHub's
[private vulnerability reporting form](https://github.com/Daniel-Vaz/KaaS-Demo/security/advisories/new).**
Please do not open a public issue, pull request or discussion for a security problem - a fix should
be available before the details are.

Useful things to include:

- **What an attacker gains** - the boundary crossed, not just the faulty behaviour. "A read-role
  group member can scale a workload" is a report; "this endpoint returns 500" usually is not.
- **Which component**: the API, the portal, the worker/reconciler, one of the sandboxes (`shell`,
  `nodessh`), an Ansible role, an OpenTofu module, the Helm chart, or the release pipeline.
- **The mode and provider** - fake mode (`make up-fake`), real mode on libvirt/vSphere/Proxmox, the
  Helm chart, or the browser demo.
- **The version** (portal sidebar footer, or `curl localhost:8081/version`) and a commit if you are
  on `main`.
- **Reproduction steps**, and a proof of concept if you have one. Fake mode reproduces most API and
  authorization issues with no hypervisor, which makes a report much easier to act on.
- **Redact credentials.** Logs, kubeconfigs and etcd snapshots from a real deployment carry live
  secrets - paste the finding, not your cluster.

**What to expect.** This project is maintained by one person, so response is best-effort rather than
contractual: acknowledgement within about a week, an assessment of scope and severity after that,
and a fix released as soon as it is ready. You will be credited in the advisory and release notes
unless you would rather not be. Please give a reasonable window before publishing - if a report goes
unanswered for 90 days, consider yourself free to disclose.

**Testing.** Test against **your own** deployment only. Never test against someone else's
KubeHarbor instance, and never against the hosted browser demo in a way that would affect others.
Good-faith research within those limits is welcome and will not be pursued; do not access, modify or
retain other people's data, and stop at proof of concept rather than pivoting deeper.

## Scope

### In scope

Anything that breaks a boundary the platform claims to enforce, including:

- **Tenancy.** One user reaching another user's cluster, kubeconfig, secrets, Vault path, node
  shell, or events - or a group member exceeding the read/write role they hold in that group.
- **Authorization bypass on the API.** The portal is not the gate; the API is. A padlock the wizard
  draws is cosmetic, but an API call that skips `authorizeClusterWrite`, `resolveAddons` or a quota
  check is a real finding.
- **Privilege escalation to platform admin.** `IsAdmin` is seed-only and deliberately not something
  a directory rule or a group role can grant.
- **Session forgery or fixation**, and login-throttle bypass on the public `POST /auth/login`.
- **Escaping a sandbox, or extracting a credential from one.** The `shell` sandbox is meant to hold
  no secrets; the `nodessh` sandbox holds the fleet SSH key and is meant to be incapable of starting
  a shell. Either property failing is in scope, as is smuggling arguments into the server-authored
  `ssh` command line.
- **Header or identity forgery through the tunnel** - notably a client-supplied `X-Webauth-User` /
  `X-Webauth-Role` reaching Grafana, or reaching a write-scoped app (Alertmanager, Longhorn UI) with
  a read role.
- **Injection into a shell-out**: `ansible-playbook`, `tofu`, `helm`, `kubectl`, `nsupdate`,
  `virsh`, `ssh`, or the WinRM/PowerShell path.
- **Path traversal or archive extraction flaws**, especially in the etcd snapshot bundle handling.
- **Secret leakage into a place it should not reach**: logs, the API's JSON, a Kubernetes pod spec
  or Helm value, an events row, or the browser.
- **SSRF, or the API reaching backends it should not**, including through the proxy seams.
- **Supply chain**: the release workflows, the published images and chart, or the CI gate.

### Out of scope

These are **known and intentional** - each is marked with a "production would…" note beside the code
that implements it. Reports that a *tightening* is possible are welcome as ordinary issues or PRs;
reports that rediscover one are not vulnerabilities.

- **Key management.** The at-rest AES key and the session signing key are derived from
  `KAAS_SECRET_KEY` in the environment rather than a KMS. Unset, the platform generates an ephemeral
  key and logs a warning. The LDAP bind password and the backend credentials likewise come from the
  environment.
- **Authentication and authorization model.** Local accounts with bcrypt and stateless signed-cookie
  sessions - no IdP/OIDC, no server-side revocation, and authorization is a coarse owner/admin split
  plus a per-group read/write role rather than fine-grained RBAC.
- **Deprovisioning lag.** Sessions are stateless and never re-check the directory, so disabling a
  user in AD does not evict them until their token expires (≤24h). Minted per-user cluster certs are
  likewise not revocable before their TTL.
- **The bundled Vault runs in dev mode** - auto-unsealed, fixed root token, in-memory storage.
- **Platform TLS and HA are not provided.** The API and portal serve plain HTTP and expect to sit
  behind something that terminates TLS; there is one Postgres.
- **Cluster defaults that are lab-scale on purpose**: the default cert-manager `ClusterIssuer` is
  self-signed, the MetalLB pool is a single address, the API-server audit log goes to the apiserver's
  own stdout rather than a durable sink, and WinRM defaults to NTLM rather than Kerberos.
- **Server-side use of the cluster admin kubeconfig** by the Monitoring, Security, Audit and tunnel
  seams. These run curated read-only queries the Kubernetes `view` role cannot express; the API is
  the gate, and production would mint per-app scoped tokens.
- **DNS credential breadth.** One service account can write the delegated zone; the per-cluster
  domain filter is a guardrail, not a boundary.
- **Default and demo credentials.** Fake mode seeds `admin` / `admin`, and the published
  [browser demo](https://daniel-vaz.github.io/KaaS-Demo/) signs in as `admin` / `kubeharbor`. The
  demo is the control plane compiled to WebAssembly running entirely in your own tab against
  in-memory fakes - there is no backend, and nothing there is shared with anyone else.
- **The `nodessh` sandbox running as root.** `ssh` refuses a group-readable private key, so
  containment there rests on the container holding no shell plus dropped capabilities and a
  read-only rootfs - not on the uid.
- Findings from automated scanners with no demonstrated impact, missing hardening headers on the
  portal, rate-limiting on authenticated endpoints, and self-XSS.

## Where the boundaries actually are

Useful context for anyone looking:

- **The API is the authoritative gate for every tenancy decision.** The portal renders affordances;
  it enforces nothing. Every interactive kubectl surface runs as a **per-user** credential - a
  cluster-CA-signed cert carrying the actor's login and their resolved role as a Kubernetes group -
  so a reader's mutation is both refused and attributed.
- **The privileged worker is deliberately separated from anything user-driven.** It holds the
  libvirt socket, the database, `KAAS_SECRET_KEY` and the Vault management token. Interactive
  sessions land in their own unprivileged sandboxes instead: `shell` (bash + kubectl, no secrets at
  all) and `nodessh` (the SSH key, but no shell). The API never receives provisioning credentials,
  and the worker never authenticates users.
- **Secrets at rest** - kubeconfigs and etcd snapshots - are sealed with `secrets.Box` before they
  reach Postgres. A snapshot is a copy of the whole cluster's Secrets plus its CA key, so it is
  exposed through no API surface at all: there is deliberately no download endpoint.

## Hardening a deployment

If you run KubeHarbor for real, at minimum:

1. **Set `KAAS_SECRET_KEY`** to a strong random value and keep it stable - unset means an ephemeral
   key and secrets that do not survive a restart.
2. **Terminate TLS in front of the portal and API**, and do not publish the API (`:8081`) to an
   untrusted network.
3. **Point `KAAS_VAULT_*` at a real Vault** rather than the bundled dev-mode one, and scope its
   tokens.
4. **Set `KAAS_ADMIN_PASSWORD`** - it defaults to `admin`, and the seeded local account is
   break-glass by design (it is tried before the directory, so a DC outage cannot lock you out).
   Prefer directory authentication (`KAAS_AUTH=ldap`) for everyone else.
5. **Treat the worker host as sensitive** - it is effectively root on every cluster you provision.
6. **Keep the SSH key, backend credentials and bind password out of version control**; `.env` is
   gitignored for a reason.

The [configuration reference](docs/deploy/configuration.md) documents every variable, and the
[operator guide](docs/deploy/README.md) covers deployment.

## Automated security tooling

The repository runs GitHub **CodeQL** code scanning, **Dependabot** security and version updates,
and **secret scanning with push protection**. Published images carry build provenance attestations.
These are a backstop, not a substitute for the reports this policy asks for.
