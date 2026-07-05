#!/usr/bin/env bash
# Delete every cluster via the running API and wait for their VMs to actually be torn down,
# so `make down` doesn't stop the worker mid-`tofu destroy` and leave orphaned libvirt domains.
# Best-effort and safe to run when nothing is up: if the API isn't reachable, it exits cleanly.
#
# Clusters are owner-scoped (internal/auth) - GET/DELETE /clusters now require an authenticated
# session, and only an admin sees/deletes every tenant's clusters. So this logs in as the seeded
# admin account (internal/app.ensureAdmin) first and reuses that session's cookie for every call.
set -u

# The API is published directly on :8081 (the portal owns :8080 and proxies /api to it).
API="${KAAS_API:-http://localhost:8081}"
WAIT_TRIES="${KAAS_TEARDOWN_TRIES:-60}"   # 60 * 5s = up to 5 min for VMs to be destroyed
ADMIN_USER="${KAAS_ADMIN_USERNAME:-admin}"
ADMIN_PASS="${KAAS_ADMIN_PASSWORD:-admin}"

COOKIEJAR="$(mktemp)"
trap 'rm -f "$COOKIEJAR"' EXIT

# Log in as the admin so we can see/delete every tenant's clusters, not just one user's.
if ! curl -sf -o /dev/null -c "$COOKIEJAR" -X POST "$API/auth/login" \
       -H 'Content-Type: application/json' \
       -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}"; then
  echo "teardown: API not reachable (or admin login failed) at $API - skipping cluster cleanup"
  exit 0
fi

# active_ids: print the id of every non-Deleted cluster, one per line. Exit 1 if the API is
# unreachable / returns junk (so the caller can distinguish "nothing to do" from "can't reach").
active_ids() {
  curl -sf -b "$COOKIEJAR" "$API/clusters" 2>/dev/null | python3 -c '
import sys, json
try:
    clusters = json.load(sys.stdin) or []
except Exception:
    sys.exit(1)
for c in clusters:
    if c.get("phase") != "Deleted":
        print(c["id"])
'
}

ids="$(active_ids)" || { echo "teardown: API not reachable at $API - skipping cluster cleanup"; exit 0; }
if [ -z "$ids" ]; then
  echo "teardown: no active clusters"
  exit 0
fi

for id in $ids; do
  echo "teardown: deleting cluster $id"
  curl -s -b "$COOKIEJAR" -X DELETE "$API/clusters/$id" >/dev/null || true
done

echo "teardown: waiting for VMs to be destroyed (up to $((WAIT_TRIES * 5))s)..."
for _ in $(seq 1 "$WAIT_TRIES"); do
  remaining="$(active_ids | grep -c . || true)"
  if [ "${remaining:-0}" = "0" ]; then
    echo "teardown: all clusters removed"
    exit 0
  fi
  sleep 5
done

echo "teardown: timed out waiting for cluster teardown - proceeding with container cleanup anyway" >&2
exit 0
