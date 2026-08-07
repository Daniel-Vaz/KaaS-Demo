# Contributing to KubeHarbor

Thanks for taking an interest. KubeHarbor is a self-hosted Kubernetes-as-a-Service control plane -
issues, questions, documentation fixes and pull requests are all welcome, and you do not need to be
running a hypervisor to contribute: **fake mode runs the entire platform with no VMs, no database
and no credentials.**

- **Bug report or feature request** → [open an issue](https://github.com/Daniel-Vaz/KaaS-Demo/issues)
- **Security vulnerability** → **do not** open an issue; see [Reporting a vulnerability](#reporting-a-vulnerability)
- **Question or "am I holding it wrong?"** → check [the documentation](docs/README.md) first, then
  open an issue

## Table of contents

- [Code of conduct](#code-of-conduct)
- [Before you start](#before-you-start)
- [Reporting bugs](#reporting-bugs)
- [Reporting a vulnerability](#reporting-a-vulnerability)
- [Suggesting features](#suggesting-features)
- [Development setup](#development-setup)
- [Making a change](#making-a-change)
- [Running the checks locally](#running-the-checks-locally)
- [Commit messages](#commit-messages)
- [Pull requests](#pull-requests)
- [Documentation changes](#documentation-changes)
- [Things maintainers own](#things-maintainers-own)
- [License](#license)

## Code of conduct

This project follows the [**Contributor Covenant**](CODE_OF_CONDUCT.md), which applies to everyone
in every project space. Please read it - and report violations privately through the
[private reporting form](https://github.com/Daniel-Vaz/KaaS-Demo/security/advisories/new) rather
than in a public issue.

The day-to-day version is short: **be decent to each other.** Assume good faith, keep criticism
about the code rather than the person, and accept that a maintainer's time is finite - an unanswered
issue is a backlog, not a snub.

## Before you start

Two things will save you the most time:

- [**`docs/`**](docs/README.md) describes the platform **as it is today**, split by audience -
  [concepts](docs/concepts/architecture.md) (how it is built), [deploy](docs/deploy/README.md)
  (running it), [portal](docs/portal/README.md) (using it).
- **The code comments carry the reasoning.** Load-bearing decisions are explained next to the code
  that implements them, and the deliberate lab-scale shortcuts are marked with a "production
  would…" note. If a change of yours seems to contradict one, read that comment before writing the
  patch - and if the reasoning is wrong or out of date, say so in the issue. That is a perfectly
  good contribution on its own.

The single idea to internalize: **this is a control plane driven by a level-triggered reconciliation
loop, not a web form that runs a script.** Desired state lives in Postgres, the API only writes
desired state, and a reconciler advances each cluster one **idempotent** step at a time - every step
is retried on failure and re-run on every tick. Changes that reconcile clusters need to hold that
line.

## Reporting bugs

A good report is one someone can reproduce. Please include:

- **What you expected, what happened**, and the steps in between.
- **The mode you are in** - fake (`make up-fake`), real (`make up`), scaled (`make up-scale`), the
  Helm chart, or the [browser demo](https://daniel-vaz.github.io/KaaS-Demo/).
- **The infrastructure provider**, if real: libvirt/KVM, vSphere, or Proxmox VE.
- **The version** - the portal shows it in the sidebar footer, or `curl localhost:8081/version`.
- **Where the cluster got stuck**: its phase, plus the cluster's Activity tab, and the API/worker
  logs (`make logs`) around the failure. For provisioning failures the OpenTofu or
  `ansible-playbook` output in the worker log is usually the whole story.
- **Whether fake mode reproduces it.** If it does, that is the single most useful sentence in the
  report - it means nobody needs your hypervisor to chase it.

**Redact before pasting.** Logs and kubeconfigs can carry cluster credentials, and an etcd snapshot
or a Vault path is sensitive by definition.

## Reporting a vulnerability

Please **do not** open a public issue for a security problem. Use GitHub's
[private vulnerability reporting](https://github.com/Daniel-Vaz/KaaS-Demo/security/advisories/new)
so a fix can go out before the details do.

The [**security policy**](SECURITY.md) has the full detail - what is in scope, the set of weaknesses
that are **deliberate, documented lab-scale shortcuts** rather than defects, and what to expect
after you report. Read it before filing: anything crossing a tenancy boundary is very much in scope,
while rediscovering the env-derived encryption key is not.

## Suggesting features

Open an issue describing **the problem** before the solution, and say which layer it lands in: the
portal, the API, the reconcile loop, a provider, or the catalog.

Two shortcuts worth knowing:

- **Kubernetes versions, OS images, add-ons and release bundles are data**, not code. They live in
  [`internal/catalog/catalog.json`](internal/catalog/catalog.json) - adding an add-on or a version
  bundle is an edit to that file, not a new feature.
- **New backends plug into an existing seam.** Every external dependency sits behind an interface
  with a fake and a real implementation selected by an environment variable ([the seam
  table](docs/concepts/architecture.md#the-seams-fake-vs-real) lists all of them). A new hypervisor
  is a `provision.Provisioner`; a new DNS backend is a `dns.Registrar`. Keeping that seam intact is
  what keeps the platform runnable
  and testable with no infrastructure at all - **anything new that reaches outside the process
  needs a fake as well as a real implementation.**

## Development setup

You need Go (the version in [`go.mod`](go.mod), currently **1.26**) and, for portal work, **Node
20**. That is enough for `make build test vet` and the whole test suite.

For the running stack you also want **Podman** with `podman compose`. Real mode additionally needs
libvirt/KVM (or vCenter / Proxmox credentials), OpenTofu, Ansible and Helm - all of which are baked
into the worker image, so you only need them on the host if you run the worker outside a container.

```bash
git clone https://github.com/Daniel-Vaz/KaaS-Demo.git
cd KaaS-Demo

make up-fake     # portal on :8080, API on :8081 - sign in as admin / admin
make help        # every target
```

Other ways in, depending on what you are changing:

| Working on | Run |
|---|---|
| Go, API + reconciler, no containers | `make run-api` (fake seams, in-memory store) |
| The reconcile loop headless | `make run-worker` (seeds a cluster, logs it converging) |
| The portal | `make web-dev` (Vite on :5173, proxying `/api`) alongside `make run-api` |
| The browser (WebAssembly) demo | `make demo-dev` |
| Real VMs | copy [`.env.example`](.env.example) to `.env`, then `make up` |

`make down` deletes any running clusters before stopping the stack, then removes every container
(Harbor included) and prunes every volume - a full cleanup, so the next `make up` starts from an
empty database. Please use it rather than `podman rm` - a stack removed underneath live clusters
leaves orphaned VMs behind.

## Making a change

- **Branch off `main`.** One logical change per branch.
- **Match the surrounding code** - its naming, its comment density, its idioms. This codebase
  comments the *why*, not the *what*; a comment explaining a non-obvious constraint is welcome, a
  comment restating the line below it is not.
- **Keep every reconcile step idempotent.** It will be re-run on the next tick and retried after a
  failure. If your step is not safe to run twice, it is not finished.
- **Respect the horizontal-scaling rules** if you touch background work: per-cluster work goes in
  the River job queue (never a ticker), singleton sweeps run under the leader lease, read-then-write
  admission takes `LockAdmission`, and nothing may be pinned to one replica. [Horizontal
  scaling](docs/concepts/architecture.md#horizontal-scaling) has the full four.
- **Store locks do not nest** - a nested acquire hangs forever.
- **JSON is `snake_case`** (the domain types carry the tags), and the API serves JSON + SSE only -
  there are no HTML routes.
- **Ansible uses only `ansible.builtin`**, so `--syntax-check` works with no extra collections
  installed. Please keep it that way.
- **Add tests** for anything with a decision in it. Policy decisions are deliberately written as
  pure functions (`domain.EtcdDefragPolicy`, `domain.RepairPolicy`, `quota`, `netpool`) precisely so
  they can be tested without a cluster - if you are adding a rule, put it there and unit-test it.
- **Update the docs in the same PR** when behaviour changes - see [below](#documentation-changes).
- **Do not commit** `.env`, kubeconfigs, keys, or anything from a real deployment.

## Running the checks locally

CI runs on every PR and is the same gate a release has to pass. It needs no hypervisor, database or
secrets, so you can run all of it locally:

```bash
make build test vet                      # Go: build, test, vet
gofmt -l .                               # must print nothing
GOOS=js GOARCH=wasm go build -o /dev/null ./cmd/demo-wasm   # the browser demo still compiles
make release-check                       # VERSION / Chart.yaml appVersion / package.json agree

cd web/portal && npm ci --legacy-peer-deps && npm run build   # type-check + bundle
make helm-lint                           # lint + render the chart in every mode

cd infra/libvirt && tofu fmt -check -recursive && tofu init -backend=false && tofu validate
                                         # repeat for infra/vsphere and infra/proxmox

cd ansible && for pb in playbooks/*.yml; do ansible-playbook --syntax-check "$pb"; done
```

The wasm build is worth running by hand if you touched anything near the exec agents or the shell
PTY: four packages have build-tagged `js/wasm` counterparts that an ordinary `go build` never
exercises.

Real-libvirt regression tests are gated behind `KAAS_TEST_LIBVIRT=1` and skipped by default.

## Commit messages

Lowercase, imperative, and specific about **what changed and why** - the repo's existing history is
the reference:

```
fix zip-slip false positive in the etcd snapshot bundle unpacker by refusing traversing entries up front
split the ESO-facing Vault address into KAAS_VAULT_CLUSTER_ADDR
port the libvirt module to the dmacvicar/libvirt 0.9 schema
```

A one-line subject is fine. Use the body for the reasoning when the change is subtle - and if the
reasoning is load-bearing, put it in a comment next to the code, where the next person will actually
find it.

## Pull requests

1. **Open it against `main`**, and describe what it changes and how you verified it. Link the issue
   it closes.
2. **Make sure CI is green.** Every job is reproducible locally (above).
3. **Label it.** Release notes are generated from merged PRs, grouped by label - so a label is the
   only per-change work a release needs. Use one of:

   `feature` · `enhancement` · `bug` · `fix` · `security` · `provider` · `infra` · `ansible` ·
   `catalog` · `documentation` · `dependencies` · `ci` · `chore`

   An unlabelled PR still shows up, under "Other changes" - forgetting one costs tidiness, never
   coverage. `ci`, `chore` and `skip-changelog` keep a PR out of the notes entirely.
4. **Screenshots for portal changes**, please - the portal docs are image-heavy and reviewing a UI
   change from a diff is guesswork.
5. **Expect review.** [`CODEOWNERS`](.github/CODEOWNERS) requests it automatically. Push follow-up
   commits rather than force-pushing over a review in progress; squashing happens at merge.

Draft PRs are a good way to get direction early on something large. For anything that changes the
state machine, a seam interface, or the database schema, an issue first will save you rework.

## Documentation changes

Docs are a first-class contribution here, and they follow one rule that is easy to miss:

- [**`docs/`**](docs/README.md) describes the platform **as-is, in present tense**, split by
  audience. It deliberately does *not* carry rationale.
- **The code comments** carry the **why** - the load-bearing decisions and the deliberate shortcuts.

So a behaviour change usually touches both: the docs say what it now does, the comment beside the
change says why it does it that way. New environment variables belong in
[`docs/deploy/configuration.md`](docs/deploy/configuration.md) and in
[`.env.example`](.env.example).

## Things maintainers own

- **Releases are tag-driven** and cut by a maintainer: `v1.4.0` publishes the five images,
  `chart-v0.3.0` the Helm chart. Nothing publishes from a laptop; the tag is the record of what
  shipped. See [Releasing](docs/deploy/releasing.md).
- **Never bump a version by hand.** [`VERSION`](VERSION) is the single source of truth and both
  `Chart.yaml`'s `appVersion` and the portal's `package.json` mirror it - `make bump VERSION=x.y.z`
  rewrites all three, and `make release-check` is the CI backstop. Version bumps do not belong in
  a feature PR at all.
- **Dependency updates arrive via Dependabot** ([`.github/dependabot.yml`](.github/dependabot.yml)).
  A manual bump PR is fine when something is broken or a Dependabot batch needs a code fix to land -
  otherwise let the bot do it.

## License

KubeHarbor is licensed under the [Apache License 2.0](LICENSE). By contributing, you agree that your
contributions are licensed under it as well.
