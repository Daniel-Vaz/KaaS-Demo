#!/usr/bin/env python3
"""Checks internal/catalog/catalog.json add-on chart versions against the latest version
available in each add-on's Helm repo, and optionally rewrites the catalog in place.

Uses `helm show chart <chart> --repo <repo>`, which resolves the latest chart version
straight from the repo's index without touching the local `helm repo add` state.

Usage:
    scripts/update-catalog-versions.py            # report only
    scripts/update-catalog-versions.py --check    # report only, exit 1 if anything is outdated (CI)
    scripts/update-catalog-versions.py --write    # rewrite catalog.json with the latest versions
"""
import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

CATALOG_PATH = Path(__file__).resolve().parent.parent / "internal" / "catalog" / "catalog.json"


def latest_version(chart: str, repo: str) -> str:
    cmd = ["helm", "show", "chart", chart]
    if repo:
        cmd += ["--repo", repo]
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    if proc.returncode != 0:
        raise RuntimeError((proc.stderr or proc.stdout).strip().splitlines()[-1] if proc.stderr or proc.stdout else "helm show chart failed")
    m = re.search(r'^version:\s*(\S+)', proc.stdout, re.MULTILINE)
    if not m:
        raise RuntimeError("no `version:` field in `helm show chart` output")
    return m.group(1).strip('"\'')


def normalize(version: str) -> str:
    # Some charts (cert-manager, envoy-gateway) publish a "v"-prefixed Chart.yaml version while
    # the catalog pins the bare form (or vice versa); Helm's own version matching already treats
    # them as equal, so comparisons here should too, or every check reports a no-op "update".
    return version[1:] if version[:1] in ("v", "V") and version[1:2].isdigit() else version


def rewrite_version(raw: str, name: str, old: str, new: str) -> tuple[str, bool]:
    # Scoped to this add-on's object: match from its "name" key up to the first "version" key
    # that follows (every catalog entry lists version shortly after name, well before "values").
    pattern = re.compile(
        r'("name":\s*"' + re.escape(name) + r'"[\s\S]*?"version":\s*")' + re.escape(old) + r'(")'
    )
    new_raw, n = pattern.subn(lambda m: m.group(1) + new + m.group(2), raw, count=1)
    return new_raw, n == 1


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--write", action="store_true", help="rewrite catalog.json with the latest versions")
    ap.add_argument("--check", action="store_true", help="exit 1 if any add-on is out of date (no changes made)")
    args = ap.parse_args()

    raw = CATALOG_PATH.read_text()
    catalog = json.loads(raw)

    updates: list[tuple[str, str, str]] = []
    skipped: list[tuple[str, str]] = []
    errors: list[tuple[str, str]] = []

    for addon in catalog["addons"]:
        name, chart, repo, old = addon["name"], addon["chart"], addon.get("repo", ""), addon["version"]
        if chart.startswith("oci://"):
            skipped.append((name, 'OCI chart - helm has no reliable "latest" for OCI refs; check manually'))
            continue
        try:
            new = latest_version(chart, repo)
        except Exception as e:
            errors.append((name, str(e)))
            continue
        if normalize(new) != normalize(old):
            updates.append((name, old, new))

    if updates:
        print("updates available:")
        for name, old, new in updates:
            print(f"  {name}: {old} -> {new}")
    else:
        print("all checked add-ons are up to date.")
    if skipped:
        print("skipped:")
        for name, reason in skipped:
            print(f"  {name}: {reason}")
    if errors:
        print("errors:", file=sys.stderr)
        for name, err in errors:
            print(f"  {name}: {err}", file=sys.stderr)

    if args.write:
        if not updates:
            print("nothing to write.")
        else:
            for name, old, new in updates:
                raw, ok = rewrite_version(raw, name, old, new)
                if not ok:
                    print(f"  WARNING: could not locate a unique version field for {name!r} - left unchanged", file=sys.stderr)
            CATALOG_PATH.write_text(raw)
            print(f"wrote {len(updates)} update(s) to {CATALOG_PATH}")
            print('note: the "bundles" section pins its own addon versions independently - bump a '
                  "bundle's pinned versions by hand if you want it to pick these up.")

    if args.check and updates:
        return 1
    if errors and not updates and not skipped:
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
