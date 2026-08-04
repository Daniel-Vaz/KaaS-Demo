#!/usr/bin/env python3
"""The platform version's single source of truth, and the check that keeps it that way.

The root VERSION file holds the platform version. Two other files MIRROR it and must never drift:

    deploy/helm/kaas/Chart.yaml   appVersion   the platform the chart deploys
    web/portal/package.json       version      the portal shipped in the web image

A drifted mirror is not a cosmetic problem: the chart's appVersion is what `kaas.image` resolves
image tags from (deploy/helm/kaas/templates/_helpers.tpl), so a stale appVersion makes `helm install`
pull a version of the platform nobody released. This script is why the release guide is a mechanism
rather than a checklist - `make release-check` runs it, CI runs it on every PR, and the release
workflow runs it AGAIN against the pushed tag, so a bad version fails before anything is published.

Deliberately NOT mirrored:

    deploy/helm/kaas/Chart.yaml   version      the CHART's own version, released on its own tag line

Usage:
    scripts/version.py                  # print the platform version
    scripts/version.py --check          # verify every mirror matches VERSION (exit 1 on drift)
    scripts/version.py --check 1.4.0    # ...and that VERSION is exactly 1.4.0 (the tag guard)
    scripts/version.py --set 1.4.0      # rewrite VERSION and every mirror
    scripts/version.py --chart-version  # print the CHART version (Chart.yaml `version`)

Edits are done with anchored line-level regexes rather than a YAML/JSON round-trip on purpose: both
files are hand-maintained and heavily commented, and reserialising them would reflow comments and
key order into an unreviewable diff.
"""
import argparse
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
VERSION_FILE = ROOT / "VERSION"
CHART_FILE = ROOT / "deploy" / "helm" / "kaas" / "Chart.yaml"
PACKAGE_FILE = ROOT / "web" / "portal" / "package.json"

# A release version: MAJOR.MINOR.PATCH with an optional prerelease suffix (1.4.0-rc.1). Kept
# deliberately tighter than full semver - no build metadata, because a `+` is not legal in a
# container image tag and every version here becomes one.
SEMVER = re.compile(r"^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$")

# Each mirror: (label, path, pattern matching the whole line, template for the rewritten line).
# The patterns are anchored to the start of the line so a nested or commented-out key can't match.
MIRRORS = [
    (
        "Chart.yaml appVersion",
        CHART_FILE,
        # `[ \t]*$`, never `\s*$`: under re.MULTILINE a `\s` run swallows the newline and the blank
        # line after it, so the rewrite would silently reflow the file around the key it edits.
        re.compile(r'^appVersion:[ \t]*"?([^"\s]+)"?[ \t]*$', re.MULTILINE),
        'appVersion: "{v}"',
    ),
    (
        "package.json version",
        PACKAGE_FILE,
        re.compile(r'^(?P<indent>[ \t]*)"version":[ \t]*"([^"]+)",[ \t]*$', re.MULTILINE),
        '{indent}"version": "{v}",',
    ),
]

CHART_VERSION = re.compile(r"^version:\s*(\S+)\s*$", re.MULTILINE)


def die(msg: str) -> None:
    print(f"version: {msg}", file=sys.stderr)
    sys.exit(1)


def read_version() -> str:
    if not VERSION_FILE.exists():
        die(f"{VERSION_FILE.relative_to(ROOT)} is missing - it is the source of truth")
    v = VERSION_FILE.read_text(encoding="utf-8").strip()
    if not v:
        die("VERSION is empty")
    return v


def read_chart_version() -> str:
    m = CHART_VERSION.search(CHART_FILE.read_text(encoding="utf-8"))
    if not m:
        die(f"no `version:` key in {CHART_FILE.relative_to(ROOT)}")
    return m.group(1).strip('"')


def mirror_values():
    """Yield (label, path, found_version) for every mirror, dying if a key has gone missing."""
    for label, path, pattern, _ in MIRRORS:
        m = pattern.search(path.read_text(encoding="utf-8"))
        if not m:
            die(f"could not find the version key for {label} in {path.relative_to(ROOT)}")
        # group(1) is the value for Chart.yaml; package.json's first group is the indent.
        yield label, path, m.group(2) if "indent" in pattern.groupindex else m.group(1)


def cmd_check(expected) -> int:
    version = read_version()
    problems = []

    if not SEMVER.match(version):
        problems.append(f"VERSION is {version!r}, which is not MAJOR.MINOR.PATCH[-prerelease]")

    if expected is not None and version != expected:
        problems.append(
            f"VERSION is {version}, but the release being cut is {expected} - "
            f"run `scripts/version.py --set {expected}`, commit, and re-tag"
        )

    for label, path, found in mirror_values():
        if found != version:
            problems.append(
                f"{label} is {found}, expected {version} ({path.relative_to(ROOT)})"
            )

    chart = read_chart_version()
    if not SEMVER.match(chart):
        problems.append(
            f"Chart.yaml version is {chart!r}, which is not MAJOR.MINOR.PATCH[-prerelease]"
        )

    if problems:
        print("version: the platform version has drifted:", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        print(
            "\nVERSION is the source of truth. Fix with `make bump VERSION=<x.y.z>`;"
            "\nsee docs/deploy/releasing.md.",
            file=sys.stderr,
        )
        return 1

    print(f"version: {version} - VERSION, Chart.yaml appVersion and package.json agree")
    print(f"version: chart {chart} (versioned independently, released on `chart-v*`)")
    return 0


def cmd_set(new: str) -> int:
    if not SEMVER.match(new):
        die(f"{new!r} is not MAJOR.MINOR.PATCH[-prerelease]")

    old = read_version() if VERSION_FILE.exists() else "(none)"
    VERSION_FILE.write_text(new + "\n", encoding="utf-8")
    print(f"  VERSION                     {old} -> {new}")

    for label, path, pattern, template in MIRRORS:
        text = path.read_text(encoding="utf-8")
        named = "indent" in pattern.groupindex

        def repl(m, template=template, named=named):
            return template.format(v=new, indent=m.group("indent") if named else "")

        updated, n = pattern.subn(repl, text, count=1)
        if n == 0:
            die(f"could not find the version key for {label} in {path.relative_to(ROOT)}")
        path.write_text(updated, encoding="utf-8")
        print(f"  {label:<27} -> {new}")

    print(
        f"\nThe chart's own version ({read_chart_version()}) is untouched - bump it in "
        "Chart.yaml\nonly when the deployment surface changes. See docs/deploy/releasing.md."
    )
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    g = ap.add_mutually_exclusive_group()
    g.add_argument(
        "--check",
        nargs="?",
        const="",
        metavar="EXPECTED",
        help="verify the mirrors match VERSION (and, given EXPECTED, that VERSION is it)",
    )
    g.add_argument("--set", metavar="X.Y.Z", help="rewrite VERSION and every mirror")
    g.add_argument(
        "--chart-version",
        action="store_true",
        help="print the CHART version (Chart.yaml `version`), not the platform version",
    )
    args = ap.parse_args()

    if args.set:
        return cmd_set(args.set)
    if args.check is not None:
        return cmd_check(args.check or None)
    if args.chart_version:
        print(read_chart_version())
        return 0
    print(read_version())
    return 0


if __name__ == "__main__":
    sys.exit(main())
