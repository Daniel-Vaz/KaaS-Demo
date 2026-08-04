# Releasing

KubeHarbor is released by **pushing a git tag**. There is no release button, no branch that publishes
itself, and no step that runs from a laptop - the tag is the record of what shipped, and everything
downstream is derived from it.

This guide is for whoever cuts a release. If you only want to *install* one, jump to
[Consuming a release](#consuming-a-release).

## What gets released

| Artifact | Where it lands |
|---|---|
| `api` | `ghcr.io/daniel-vaz/kaas-demo/api` |
| `web` | `ghcr.io/daniel-vaz/kaas-demo/web` |
| `worker` | `ghcr.io/daniel-vaz/kaas-demo/worker` |
| `shell` | `ghcr.io/daniel-vaz/kaas-demo/shell` |
| `nodessh` | `ghcr.io/daniel-vaz/kaas-demo/nodessh` |
| Helm chart | `ghcr.io/daniel-vaz/kaas-demo/charts/kaas` (OCI), plus a `.tgz` on the GitHub Release |

Each image is its **own package** - individually pullable, individually pinnable - but all five are
published from **one platform version**. They are one Go module sharing `internal/`, so a deployment
running two of them at different versions is a hazard rather than a convenience. The single exception
is the [hotfix path](#cutting-a-single-image-hotfix).

The `lb` image (`deploy/Containerfile.lb`) is **not** published: it exists only for the scaled Compose
bring-up, where it is built locally. On Kubernetes a Service does its job.

## Two version lines

The platform and the chart are versioned **independently**, because they change on different rhythms:
a template fix or a better default is a chart release with no images rebuilt, and most platform
releases need no chart change at all.

| | Platform version | Chart version |
|---|---|---|
| Source of truth | the root [`VERSION`](../../VERSION) file | `version:` in [`Chart.yaml`](../../deploy/helm/kaas/Chart.yaml) |
| Covers | the five images, the Go binaries, the portal | the chart's templates, values and defaults |
| Released by | `v1.4.0` | `chart-v0.3.0` |
| Mirrored into | `Chart.yaml` `appVersion`, `web/portal/package.json` | nothing |

What ties them together is the chart's **`appVersion`**: it names the platform version the chart
deploys, and `kaas.image` (`templates/_helpers.tpl`) resolves every image tag from it. That is why a
released chart installs with no `--set image.*` at all - and why a drifted `appVersion` is a real bug,
not a cosmetic one.

## Tag grammar

| Tag | Publishes |
|---|---|
| `v1.4.0` | all five images at `1.4.0`, `1.4` and `latest`, plus a GitHub Release |
| `v1.4.0-rc.1` | all five images at `1.4.0-rc.1` **only**, as a prerelease |
| `chart-v0.3.0` | the Helm chart, as an OCI artifact and a Release asset |
| `worker-v1.4.1` | that one image at `1.4.1`, and nothing else |

A prerelease never moves `latest` or the `X.Y` alias - handing an rc to everything that tracks them is
the opposite of what an rc is for.

---

## What to update before you tag

**Everything mirrors [`VERSION`](../../VERSION).** Never edit a mirror by hand:

```bash
make bump VERSION=1.4.0     # rewrites VERSION, Chart.yaml appVersion, package.json
make release-check          # verifies they agree - exactly what CI runs on every PR
```

| File | Field | Who updates it |
|---|---|---|
| `VERSION` | the whole file | `make bump` |
| `deploy/helm/kaas/Chart.yaml` | `appVersion` | `make bump` |
| `web/portal/package.json` | `version` | `make bump` |
| `deploy/helm/kaas/Chart.yaml` | `version` | **you, by hand** - the chart's own version, on its own tag line |

`make release-check` runs in CI on every pull request, so drift fails a PR rather than a release. The
release workflow runs the same check **again** against the pushed tag: if the tag says `1.4.0` and
`VERSION` says `1.3.0`, nothing is published.

You do not update image tags anywhere. `Makefile`'s `IMAGE_TAG` defaults to `VERSION`, and the chart's
`image.tag` is deliberately empty so `appVersion` wins.

---

## Cutting a platform release

```bash
git switch main && git pull

make bump VERSION=1.4.0
make release-check
make build test vet          # the gate CI will re-run at the tag

git commit -am "release 1.4.0"
git push origin main

git tag -a v1.4.0 -m "KubeHarbor 1.4.0"
git push origin v1.4.0
```

Then, in Actions:

1. **Prepare** parses the tag and runs the guards - the version matches `VERSION`, and the tagged
   commit is an ancestor of `main`.
2. **Verify** re-runs the whole CI gate at the tagged commit (a *call* to `ci.yml`, not a copy, so a
   release can never pass weaker checks than a PR).
3. **Approve publish** waits on the `release` environment. Nothing has reached GHCR yet.
4. **Publish** builds and pushes the five images with the version and commit stamped in, and attaches
   a signed [build-provenance attestation](#verifying-what-you-pulled) to each digest.
5. **GitHub Release** is created, with a pull-by-digest table above notes generated from the merged
   PRs (grouped by label - see [`.github/release.yml`](../../.github/release.yml)).
6. **Record deployment** files the release against the `ghcr-images` environment.

Before releasing a chart against it, sanity-check the images actually run:

```bash
helm install kaas oci://ghcr.io/daniel-vaz/kaas-demo/charts/kaas \
  --version <current chart> --set image.tag=1.4.0 --set providers=fake
```

## Cutting a chart release

The chart's `appVersion` **must** name a platform version that has already been released - the
workflow refuses otherwise, because a chart pointing at images that were never built fails at
`ImagePullBackOff` on someone else's cluster, long after the mistake.

```bash
# 1. bump `version:` in deploy/helm/kaas/Chart.yaml by hand (appVersion is already correct)
make chart-package           # lint + template + package into dist/, exactly as CI does

git commit -am "chart 0.3.0"
git push origin main

git tag -a chart-v0.3.0 -m "kaas chart 0.3.0"
git push origin chart-v0.3.0
```

Same shape as above: guards → verify → approve → `helm push` to GHCR → a Release with the `.tgz` and
its checksum → a record against `ghcr-chart`.

## Cutting a single-image hotfix

For an urgent fix confined to one image - a CVE in the worker's toolchain, a portal bug - when
re-releasing the whole platform is not warranted.

```bash
git tag -a worker-v1.4.1 -m "worker 1.4.1: <what and why>"
git push origin worker-v1.4.1
```

This publishes `worker:1.4.1` and **nothing else**: no `latest`, no `1.4`, no other image moved. A
deployment takes it with a per-component override:

```bash
helm upgrade kaas oci://ghcr.io/daniel-vaz/kaas-demo/charts/kaas \
  --version 0.3.0 --reuse-values --set image.tags.worker=1.4.1
```

> [!IMPORTANT]
> A hotfix tag deliberately skips the `VERSION` guard, so the tree still says `1.4.0` while one image
> says `1.4.1`. That is a **temporary** state and the `image.tags` override is what carries it. Fold
> the fix into the next platform release and drop the override - leaving it in place means one
> component drifting away from the four it shares `internal/` with, which is the exact hazard the
> single-version rule exists to prevent.

---

## Consuming a release

The chart is the intended path. It resolves every image from its own `appVersion`, so a version is
all you give it:

```bash
helm install kaas oci://ghcr.io/daniel-vaz/kaas-demo/charts/kaas --version 0.3.0
```

Pull an image directly if you need one:

```bash
podman pull ghcr.io/daniel-vaz/kaas-demo/api:1.4.0
podman pull ghcr.io/daniel-vaz/kaas-demo/api@sha256:...   # immutable
```

Tag precedence in the chart, most specific first:

| Value | Effect |
|---|---|
| `image.tags.<component>` | one component pinned - the hotfix path |
| `image.tag` | all five pinned together - a rollback, a local build, an rc |
| *(neither)* | `.Chart.AppVersion` - **the default**, and what a released chart is for |

### Verifying what you pulled

Every published digest carries a keyless build-provenance attestation tying it to the workflow run,
the commit and the runner:

```bash
gh attestation verify oci://ghcr.io/daniel-vaz/kaas-demo/api:1.4.0 -R Daniel-Vaz/KaaS-Demo
```

Every image also carries the standard OCI labels, so a pulled image names its own commit:

```bash
podman inspect --format '{{index .Labels "org.opencontainers.image.revision"}}' \
  ghcr.io/daniel-vaz/kaas-demo/api:1.4.0
```

### What is running right now

The API reports its own build identity, and the portal shows it at the foot of the sidebar:

```bash
curl -s http://localhost:8080/api/version
# {"version":"1.4.0","commit":"a1b2c3d...","date":"2026-08-04T10:00:00Z"}
```

`"dev"` means an unstamped build - `go run`, `make run-api`, or an image built without the build args.

### Rolling back

Pin the previous version. **Never move a published tag**: the digests are attested, deployments pull
by tag, and a moved tag makes "which version is this?" unanswerable for everyone at once.

```bash
helm upgrade kaas oci://ghcr.io/daniel-vaz/kaas-demo/charts/kaas --version 0.2.0 --reuse-values
helm upgrade kaas ... --reuse-values --set image.tag=1.3.0   # images only, chart unchanged
```

> [!WARNING]
> Rolling the platform back does **not** roll the database schema back. Migrations run forward on
> start-up and are not reversible, so a rollback across a migration needs a restore of `pgdata`. And
> never change `KAAS_SECRET_KEY` across any upgrade or rollback - it derives the key that encrypts
> every stored secret (see the [operator guide](README.md)).

---

## Environments and approvals

Three GitHub environments, created once in **Settings → Environments**:

| Environment | Purpose | Protection |
|---|---|---|
| `release` | the approval gate - every publish job waits on it | **required reviewer** |
| `ghcr-images` | the record of which platform version was published, and when | none |
| `ghcr-chart` | the same, for the chart | none |

`ghcr-images` and `ghcr-chart` are **ledgers, not deployment targets**. The Environments tab is an
accurate history of what was published; nothing in this repository deploys to a running KubeHarbor.

That is deliberate. This platform runs as Podman containers inside WSL2, which a GitHub-hosted runner
cannot reach - and giving CI a credential that *could* reach it would place a GitHub-triggered process
next to the libvirt socket, the platform's secret key and every tenant's Vault token. The release
pipeline holds nothing but a `GITHUB_TOKEN` scoped to the repo's own packages.

**Deploying for real is a documented seam, not a wired one.** Register a self-hosted runner on the
deployment host and add a job gated on its own environment:

```yaml
deploy:
  needs: [prepare, publish]
  runs-on: [self-hosted, lab]
  environment: lab              # its own approval, separate from `release`
  steps:
    - run: |
        helm upgrade --install kaas \
          oci://ghcr.io/daniel-vaz/kaas-demo/charts/kaas \
          --version ${{ needs.prepare.outputs.version }} --reuse-values
```

It needs only the chart reference and a kubeconfig on the runner - no registry credential, since the
chart and images are public. Weigh it against what a self-hosted runner means for that host first.

---

## Reference

| | |
|---|---|
| [`VERSION`](../../VERSION) | the platform version's single source of truth |
| [`scripts/version.py`](../../scripts/version.py) | the mirror check and the bump; `make release-check` / `make bump` |
| [`.github/workflows/release.yml`](../../.github/workflows/release.yml) | images + the platform Release |
| [`.github/workflows/release-chart.yml`](../../.github/workflows/release-chart.yml) | the Helm chart |
| [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) | the gate, called by both |
| [`.github/release.yml`](../../.github/release.yml) | how generated notes are grouped by label |

### Troubleshooting

| Failure | Cause |
|---|---|
| `'1.4.0' … but the release being cut is 1.4.1` | `make bump` was skipped or the wrong tag was pushed. Fix the tree, delete the tag, re-tag. |
| `points at a commit that is not on main` | the tag is on a branch. Merge first, then tag the merged commit. |
| `no v1.4.0 tag exists` (chart) | the chart's `appVersion` names a platform release that was never cut. Release the platform first. |
| `chart-v0.3.0 says 0.3.0, but Chart.yaml says 0.2.0` | bump `version:` in `Chart.yaml` and re-tag. |
| The run is stuck before publishing | it is waiting on the `release` environment. Approve it in the run's page. |

Deleting and re-pushing a tag is fine **before** anything is published (the guards run first). Once
images are pushed, cut a new patch version instead - re-tagging over published artifacts is how a tag
stops meaning anything.
