<!--
Thanks for contributing. CONTRIBUTING.md has the full guide; this template is the short version.

Anything security-sensitive should NOT arrive as a public PR - see SECURITY.md.
-->

## What this changes

<!-- What it does and, where it isn't obvious, why. One or two paragraphs is plenty. -->

Closes #

## How it was verified

<!-- Say what you actually ran or clicked. "CI is green" is not verification on its own for anything
     that touches a real cluster - fake mode covers a lot, but not provisioning. -->

- [ ] `make build test vet` and `gofmt -l .` is clean
- [ ] Ran it: <!-- make up-fake / make up on libvirt / vSphere / Proxmox / helm / browser demo -->
- [ ] Portal changes: screenshots below
- [ ] Real infrastructure changes: tested against <!-- which provider, which cluster shape -->

## Label

<!-- Release notes are generated from merged PRs grouped by label, so a label is the only per-change
     work a release needs. Pick one: feature · enhancement · bug · fix · security · provider · infra ·
     ansible · catalog · documentation · dependencies · ci · chore
     Unlabelled PRs still appear, under "Other changes". -->

## Checklist

- [ ] Branched off `main`, one logical change
- [ ] Docs updated in this PR if behaviour changed (`docs/` = what it does, code comments = why)
- [ ] New environment variables documented in `docs/deploy/configuration.md` **and** `.env.example`
- [ ] Tests added for anything with a decision in it (policies are pure functions on purpose)
- [ ] No hand-edited version bump - `VERSION` is the single source of truth, changed via `make bump`
- [ ] No secrets, kubeconfigs or `.env` contents committed

<!-- If your change touches the reconcile loop, confirm these too - they are the invariants that are
     easiest to break silently and hardest to spot in review: -->

- [ ] Reconcile steps are **idempotent** - safe to re-run on every tick and after a failure
- [ ] Per-cluster background work goes in the River job queue, not a ticker; singleton sweeps run
      under the leader lease; read-then-write admission takes `LockAdmission`; nothing is pinned to
      one replica
- [ ] Any new external dependency sits behind a seam with **both** a fake and a real implementation

## Screenshots

<!-- Required for portal changes - reviewing a UI diff without them is guesswork. Before/after if
     you are changing something that already exists. -->
