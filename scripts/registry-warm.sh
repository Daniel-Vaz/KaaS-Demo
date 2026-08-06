#!/usr/bin/env bash
# Warm the platform registry's pull-through caches with the images the default bundle's add-ons pull.
#
# WHY THIS EXISTS, AND WHY IT IS NOT A RECONCILE STEP.
#
# The proxy cache populates itself: the first cluster to install an add-on pulls its images through
# the cache, and every cluster after that gets them over the LAN. That leaves exactly one gap - the
# FIRST cluster, which pays the cache-miss penalty (an extra hop plus a cache write) on top of the
# download it was going to do anyway. This script closes that gap by doing those pulls once, ahead of
# time, from wherever it is run.
#
# It stays a make target rather than becoming reconcile work because it is deployment-time hygiene,
# not desired state: nothing is broken if it never runs, and nothing drifts if it does.
#
# The image list is DERIVED, never curated. `helm template` renders each add-on exactly as the
# platform installs it and the images fall out of the rendered manifests, so a catalog version bump
# needs no edit here. That is the whole reason this is worth having and a hand-maintained list of
# images would not be: a list that goes stale silently is worse than no list.
#
#   scripts/registry-warm.sh                 # the default bundle's add-ons
#   scripts/registry-warm.sh cilium longhorn # just these
#
# Needs: helm, and a container runtime that can pull (podman or docker). Reads the registry host from
# KAAS_REGISTRY_HOST (or the .env in the repo root).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CATALOG="$ROOT/internal/catalog/catalog.json"

if [[ -f "$ROOT/.env" ]]; then
  # shellcheck disable=SC1091
  set -a; source "$ROOT/.env"; set +a
fi

HOST="${KAAS_REGISTRY_HOST:-}"
PREFIX="${KAAS_REGISTRY_PROJECT_PREFIX:-kaas-}"
if [[ -z "$HOST" ]]; then
  echo "registry-warm: set KAAS_REGISTRY_HOST to the registry's node-facing address (e.g. 192.168.1.20:8090)" >&2
  exit 1
fi

for tool in helm jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "registry-warm: $tool is required" >&2; exit 1; }
done
RUNTIME="$(command -v podman || command -v docker)" || {
  echo "registry-warm: podman or docker is required" >&2; exit 1; }

# upstream_project maps an image's registry host to the cache project the platform created for it.
# It mirrors registry.DefaultUpstreams - the one place this script duplicates knowledge, and it is
# four lines rather than a hundred image names.
upstream_project() {
  case "$1" in
    docker.io|index.docker.io|registry-1.docker.io) echo "${PREFIX}cache-dockerhub" ;;
    ghcr.io)                                        echo "${PREFIX}cache-ghcr" ;;
    quay.io)                                        echo "${PREFIX}cache-quay" ;;
    registry.k8s.io|k8s.gcr.io)                     echo "${PREFIX}cache-k8s" ;;
    *)                                              echo "" ;;
  esac
}

# addons lists the add-on names to warm: the arguments, or every add-on in the catalog's newest
# supported bundle - which is what a cluster created today actually installs. Read from the catalog
# so this follows the platform rather than a copy of it.
addons() {
  if [[ $# -gt 0 ]]; then printf '%s\n' "$@"; return; fi
  jq -r 'first(.bundles[] | select(.status == "supported")) | .addons | keys[]' "$CATALOG"
}

# bundle_version is the version the current bundle PINS for an add-on, which may differ from the
# catalog entry's own. Warming the wrong version caches images nothing will pull.
bundle_version() {
  jq -r --arg n "$1" 'first(.bundles[] | select(.status == "supported")) | .addons[$n] // ""' "$CATALOG"
}

# images_for renders one add-on's chart and pulls every image reference out of the manifests. Charts
# name images in a dozen shapes, so this greps the rendered YAML rather than trying to know each
# chart's values schema - the rendered output is the one place they all agree.
images_for() {
  local name="$1" repo chart version
  repo="$(jq -r --arg n "$name" '.addons[] | select(.name==$n) | .repo' "$CATALOG")"
  chart="$(jq -r --arg n "$name" '.addons[] | select(.name==$n) | .chart' "$CATALOG")"
  version="$(bundle_version "$name")"
  [[ -n "$version" ]] || version="$(jq -r --arg n "$name" '.addons[] | select(.name==$n) | .version' "$CATALOG")"
  [[ "$chart" != "null" && -n "$chart" ]] || return 0

  local args=()
  if [[ "$chart" == oci://* ]]; then
    args=("$chart" --version "$version")
  else
    helm repo add "kaas-warm-$name" "$repo" >/dev/null 2>&1 || true
    helm repo update "kaas-warm-$name" >/dev/null 2>&1 || true
    args=("kaas-warm-$name/$chart" --version "$version")
  fi
  helm template warm "${args[@]}" 2>/dev/null \
    | grep -oE '^[[:space:]]*-?[[:space:]]*image:[[:space:]]*"?[^"[:space:]]+' \
    | sed -E 's/.*image:[[:space:]]*"?//' \
    | grep -vE '^\{\{|^$' \
    | sort -u
}

pulled=0 skipped=0
while read -r addon; do
  [[ -n "$addon" ]] || continue
  echo "==> $addon"
  while read -r image; do
    [[ -n "$image" ]] || continue
    # Split "host/path:tag". An image with no dotted first segment is a Docker Hub short name
    # ("nginx:1.2", "grafana/grafana:11"), which is the default when no host is given.
    host="${image%%/*}"
    if [[ "$host" != *.* && "$host" != *:* ]] || [[ "$image" != */* ]]; then
      host="docker.io"; path="$image"
    else
      path="${image#*/}"
    fi
    project="$(upstream_project "$host")"
    if [[ -z "$project" ]]; then
      echo "    skip $image (no cache project for $host)"
      skipped=$((skipped + 1))
      continue
    fi
    # Pulling THROUGH the cache is what populates it - the point is not to hold the image locally.
    ref="$HOST/$project/$path"
    echo "    warm $ref"
    if "$RUNTIME" pull --quiet "$ref" >/dev/null 2>&1; then
      pulled=$((pulled + 1))
      # Drop the local copy immediately: this host is not where the images are meant to live, and a
      # warm-up that fills the developer's disk with the whole bundle is its own bug.
      "$RUNTIME" rmi "$ref" >/dev/null 2>&1 || true
    else
      echo "      failed (the cache may not proxy this upstream, or the tag does not exist)"
      skipped=$((skipped + 1))
    fi
  done < <(images_for "$addon")
done < <(addons "$@")

# Tidy the temporary helm repos, so a repeat run does not accumulate them.
helm repo list 2>/dev/null | awk '/^kaas-warm-/ {print $1}' | xargs -r -n1 helm repo remove >/dev/null 2>&1 || true

echo
echo "registry-warm: $pulled image(s) cached, $skipped skipped."
echo "The next cluster's add-on installs pull these over the LAN instead of the internet."
