#!/usr/bin/env bash
# Bring Harbor - the platform's container image registry (internal/registry) - up or down.
#
# Harbor is deployed the way Harbor is meant to be deployed: its own installer's `prepare` step reads
# deploy/harbor/harbor.yml and generates the compose file plus every internal config (nginx, the
# registry, the token service, the database bootstrap). This script drives that and nothing more.
#
# The alternative - vendoring Harbor's eight services and their generated configs into this repo -
# would mean maintaining a fork of somebody else's deployment and re-deriving it on every Harbor
# release, for no benefit. See deploy/compose.harbor.yaml, which carries only the platform's own
# KAAS_REGISTRY_* wiring.
#
#   scripts/harbor.sh up      download (once) + prepare + start
#   scripts/harbor.sh ensure  same, but a NO-OP when unconfigured or already running
#   scripts/harbor.sh down    stop, keeping the data volume
#   scripts/harbor.sh stop    same, but a NO-OP when there is no Harbor here
#   scripts/harbor.sh purge   stop and DELETE Harbor's state - every image AND its database
#   scripts/harbor.sh reap    same, but a NO-OP when there is no Harbor here
#
# `ensure` and `reap` are the pair every `make up` / `make down` runs, so Harbor's lifecycle follows
# the platform's. Neither may ever be what breaks a bring-up or a teardown: a host with no
# harbor.yml, or one where Harbor fails to come up, still gets a working control plane - with the
# registry seam simply reporting itself unreachable.
#
# `down`/`stop` are NOT destructive: Harbor keeps its state in harbor.yml's `data_volume`, a host
# directory that stopping containers does not touch, so the pull-through cache survives and is warm
# again on the next `up`. That is also why stopping is not enough for a teardown that calls itself a
# full cleanup - `make down` reaps instead, and `make down KEEP_CACHE=1` opts back into stopping.
set -euo pipefail

HARBOR_VERSION="${HARBOR_VERSION:-v2.15.1}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR="$ROOT/deploy/harbor"
DIST="$DIR/harbor"            # the extracted installer (gitignored)
CFG="$DIR/harbor.yml"

# Podman's docker-compose shim, or docker compose - whichever this machine has. Harbor's installer
# calls `docker compose`, so a shim has to be on PATH under that name; when it is not, we drive the
# generated compose file ourselves, which works identically.
compose() {
  if command -v podman-compose >/dev/null 2>&1; then
    podman-compose "$@"
  elif docker compose version >/dev/null 2>&1; then
    docker compose "$@"
  else
    echo "harbor: need podman-compose or docker compose on PATH" >&2
    exit 1
  fi
}

using_podman() { command -v podman-compose >/dev/null 2>&1; }

# depodmanize rewrites the compose file `prepare` just generated, for the one place Harbor's installer
# assumes docker and podman disagrees: every service asks for the `syslog` LOG DRIVER, shipping its
# logs to the harbor-log sidecar. Podman implements no such driver - only k8s-file, journald, none and
# passthrough - so under podman every service except harbor-log itself dies at create time with
#
#   Error: running container create option: invalid log driver: invalid argument
#
# which is reported per-container and then buried under a cascade of "X is not a valid container,
# cannot be used as a dependency", so the actual cause is four screens up.
#
# Dropping the block entirely rather than naming a replacement driver: podman then uses its own
# default (k8s-file) and `podman logs harbor-core` works, which on this platform is more useful than a
# syslog sidecar nothing reads. It is left ALONE under docker, where the sidecar is Harbor's intended
# design and works as shipped.
#
# Patching generated output is not a fork: `prepare` rewrites this file from harbor.yml on every run,
# so the transform re-applies itself and there is nothing to keep in sync. (Contrast harbor.yml.example,
# which is upstream's template verbatim precisely because it is an INPUT.)
depodmanize() {
  local f="$DIST/docker-compose.yml"
  using_podman || return 0
  grep -q 'driver: "syslog"' "$f" 2>/dev/null || return 0
  echo "harbor: podman detected - dropping the syslog log driver from the generated compose file"
  # Delete each `logging:` key and every line indented deeper than it.
  awk '
    /^[ \t]*logging:[ \t]*$/ { ind = match($0, /[^ \t]/) - 1; skip = 1; next }
    skip {
      if ($0 ~ /^[ \t]*$/) next
      if (match($0, /[^ \t]/) - 1 > ind) next
      skip = 0
    }
    { print }
  ' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
}

need_config() {
  if [[ ! -f "$CFG" ]]; then
    cat >&2 <<EOF
harbor: $CFG does not exist.

  cp deploy/harbor/harbor.yml.example deploy/harbor/harbor.yml
  \$EDITOR deploy/harbor/harbor.yml

The one setting that matters is 'hostname': it must be an address a CLUSTER NODE can reach (a LAN
address, never localhost), because that is what ends up in every image reference. Put the same value
in KAAS_REGISTRY_HOST.
EOF
    exit 1
  fi
}

# need_host_dirs creates the host directories harbor.yml bind-mounts: `data_volume` (images) and
# `log.local.location` (the syslog container's output). Harbor's generated compose file mounts both,
# and PODMAN refuses a missing bind-mount source with a bare "statfs ...: no such file or directory" -
# a confusing way to be told to run mkdir, and one that surfaces mid-startup, after half the services
# have already been created. Both upstream defaults (/data, /var/log/harbor) need root, so when we
# cannot create one ourselves we say exactly what to run rather than guessing at sudo inside `make up`.
#
# Parsed with awk rather than a YAML parser deliberately: this is two well-known keys in a file we
# ship the template for, and requiring python-yaml to run `make up` is a worse trade.
need_host_dirs() {
  local vol log rc=0
  vol="$(awk '/^data_volume:/ {print $2}' "$CFG")"
  # `location` is nested under log.local; the only other one in the file is commented out.
  log="$(awk '/^log:/ {in_log=1} in_log && /^[[:space:]]+location:[[:space:]]*[^[:space:]#]/ {print $2; exit}' "$CFG")"
  need_dir "$vol" "data volume" "it is where every cached and pushed image lives, so give it real space" || rc=1
  need_dir "$log" "log directory" "it holds Harbor's own service logs" || rc=1
  return $rc
}

need_dir() {
  local dir="$1" what="$2" why="$3"
  [[ -n "$dir" ]] || return 0
  [[ -d "$dir" ]] && return 0
  if mkdir -p "$dir" 2>/dev/null; then
    echo "harbor: created $what $dir"
    return 0
  fi
  cat >&2 <<EOF
harbor: cannot create the $what $dir (it needs root).

  sudo mkdir -p $dir && sudo chown -R \$(id -u):\$(id -g) $dir

Or point it somewhere you own in $CFG - $why.
EOF
  return 1
}

# remove_data is need_host_dirs' counterpart: it deletes the two host directories harbor.yml
# bind-mounts, read from the same two keys with the same awk. That is the whole of Harbor's state -
# `data_volume` holds the registry blobs, Harbor's own Postgres database (its PROJECTS, robots and
# memberships), redis and its generated secrets, and the log directory holds its service logs.
#
# It exists because "stop the containers" is not a cleanup here. Nothing under data_volume is a
# podman volume, so `podman volume prune` cannot reach it and a torn-down platform came back up with
# the previous deployment's projects and cached images still in it.
remove_data() {
  local vol log
  vol="$(awk '/^data_volume:/ {print $2}' "$CFG")"
  log="$(awk '/^log:/ {in_log=1} in_log && /^[[:space:]]+location:[[:space:]]*[^[:space:]#]/ {print $2; exit}' "$CFG")"
  remove_dir "$vol"
  remove_dir "$log"   # usually inside data_volume, so usually already gone
}

# remove_dir rm -rf's one directory, with the guards a scripted rm -rf earns: the path is read out of
# a config file, so an empty or relative value - a mis-parse, a hand-edited harbor.yml - must never
# turn into `rm -rf` against $PWD, and `/` is refused outright.
#
# `podman unshare` is load-bearing rather than belt-and-braces. Under ROOTLESS podman the database
# and redis directories end up owned by a SUBUID (the container's postgres/redis user mapped out of
# our namespace) at mode 0700, so a plain `rm -rf` as the invoking user gets EPERM part-way in and
# leaves behind the very directory the purge exists to remove - and, being a partial delete, one that
# no longer starts. `unshare` re-enters the user namespace where those uids are ours. It is tried
# FIRST because it also handles the directories we own, so there is one path rather than two.
# A root/docker host has none of this problem (the files are root-owned and we are root), and if both
# attempts fail we say what to run instead of failing a teardown.
remove_dir() {
  local dir="$1"
  [[ -n "$dir" && "$dir" == /* && "$dir" != "/" ]] || return 0
  [[ -d "$dir" ]] || return 0
  if command -v podman >/dev/null 2>&1; then
    podman unshare rm -rf "$dir" >/dev/null 2>&1 || true
  fi
  if [[ -d "$dir" ]]; then
    rm -rf "$dir" >/dev/null 2>&1 || true
  fi
  if [[ -d "$dir" ]]; then
    echo "harbor: could not remove $dir - the container runtime owns part of it: sudo rm -rf $dir" >&2
    return 0
  fi
  echo "harbor: removed $dir"
}

fetch() {
  if [[ -d "$DIST" ]]; then
    return
  fi
  echo "harbor: downloading the $HARBOR_VERSION online installer..."
  mkdir -p "$DIR"
  curl -fsSL \
    "https://github.com/goharbor/harbor/releases/download/${HARBOR_VERSION}/harbor-online-installer-${HARBOR_VERSION}.tgz" \
    | tar xz -C "$DIR"
}

# exclusive serializes every state-changing subcommand against itself. Harbor takes minutes to come
# up and `ensure` now runs on EVERY `make up`, so a second `make up` inside that window would fork a
# competing `prepare` + `compose up` onto the same containers - which does not merely duplicate work,
# it wedges: the two runs race to create the same container names and each fails on the other's
# half-built state, leaving a deployment that is neither up nor down.
#
# It is an flock rather than a pidfile because the kernel releases it when the process dies however it
# dies - a pidfile left by a killed run is a lock nobody can clear. It locks a file of its own rather
# than harbor.yml: flock CREATES the file it is given, and an empty harbor.yml would satisfy
# need_config and then die inside `prepare` on a schema error, which is a worse failure than the race.
#
# `-o` IS LOAD-BEARING, and its absence is a permanent hang rather than a subtle bug. Without it flock
# hands the locked descriptor to the command, which hands it to every process it spawns - and podman
# spawns LONG-LIVED daemons (aardvark-dns, the network helper). That daemon outlives the bring-up
# still holding the lock, so the NEXT `make up` blocks forever on a Harbor that is already serving.
# `-o` closes the descriptor before exec, while flock's own process keeps holding the lock until the
# command exits: the mutual exclusion is unchanged, the inheritance is gone.
exclusive() {
  if [[ -n "${_HARBOR_LOCKED:-}" ]] || ! command -v flock >/dev/null 2>&1; then
    return 0   # already inside the lock (`ensure` re-invoking `up`), or no flock on this host
  fi
  export _HARBOR_LOCKED=1
  exec flock -o "$DIR/.lock" "$0" "$@"
}

# wait_up decides whether the bring-up worked, by looking at the containers rather than at
# podman-compose's exit code (see the `|| true` at the call site). Harbor's own compose file names its
# services, so "all of these are running" is the whole check.
wait_up() {
  local want="harbor-log harbor-db redis registry registryctl harbor-core harbor-portal harbor-jobservice nginx"
  local i svc missing names
  for i in $(seq 1 60); do
    names="$(ps_names)"
    missing=""
    for svc in $want; do
      grep -qx "$svc" <<<"$names" || missing="$missing $svc"
    done
    [[ -z "$missing" ]] && return 0
    sleep 2
  done
  echo "harbor: not running:$missing" >&2
  return 1
}

# ps_names lists running container names from whichever engine this host uses.
ps_names() {
  if command -v podman >/dev/null 2>&1; then
    podman ps --format '{{.Names}}' 2>/dev/null
  elif command -v docker >/dev/null 2>&1; then
    docker ps --format '{{.Names}}' 2>/dev/null
  fi
}

# wait_healthy polls Harbor's own health endpoint - the signal the platform actually depends on, since
# a running container is not yet an API that answers.
wait_healthy() {
  local host="$1" port="$2" i
  command -v curl >/dev/null 2>&1 || return 0
  for i in $(seq 1 60); do
    if curl -fsS --max-time 3 "http://${host}:${port}/api/v2.0/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 3
  done
  return 1
}

# running reports whether Harbor is already serving, so `ensure` on a warm host costs one process
# rather than a full prepare + compose up.
running() {
  ps_names | grep -qx 'harbor-core'
}

case "${1:-up}" in
  ensure)
    # Not configured: say how, and get out of the way. This is the fresh-clone path, and it must
    # leave `make up` completely unaffected.
    if [[ ! -f "$CFG" ]]; then
      echo "harbor: not configured (deploy/harbor/harbor.yml absent) - the platform will use the fake registry seam."
      echo "harbor: to enable it, cp deploy/harbor/harbor.yml.example deploy/harbor/harbor.yml and set 'hostname'."
      exit 0
    fi
    # Take the lock BEFORE the running check, not after: a concurrent `make up` that is mid-bring-up
    # holds it, and by the time we get in Harbor is up and this costs one `podman ps`.
    exclusive "$@"
    if running; then
      echo "harbor: already running"
      exit 0
    fi
    # Configured but down. Bring it up, and DO NOT fail the caller if it cannot start - a registry
    # that is down degrades pull speed and blanks one portal page; it must not block a bring-up.
    if ! "$0" up; then
      echo "harbor: failed to start - continuing without it (the Registry page will report it unreachable)" >&2
    fi
    exit 0
    ;;
  up)
    need_config
    exclusive "$@"
    need_host_dirs
    fetch
    cp "$CFG" "$DIST/harbor.yml"
    echo "harbor: generating configuration..."
    # Harbor's `prepare` is a container that bind-mounts its own output directory. DOCKER creates a
    # missing bind-mount source; PODMAN refuses, with a bare "statfs ...: no such file or directory"
    # that reads like a corrupt installer. Create it ourselves so the same installer works under both.
    mkdir -p "$DIST/common/config"
    (cd "$DIST" && ./prepare)
    depodmanize
    echo "harbor: starting..."
    # `|| true`: podman-compose exits NONZERO even when every container started - it propagates the
    # status of its last podman invocation, which for a container that was already there is not 0.
    # Under `set -e` that aborted the script after a completely successful bring-up, and (via `ensure`)
    # printed "harbor: failed to start" on a Harbor that was serving. So the outcome is decided by
    # LOOKING at the containers, not by trusting the exit code.
    (cd "$DIST" && compose -f docker-compose.yml up -d) || true
    host="$(grep -E '^hostname:' "$CFG" | awk '{print $2}')"
    port="$(grep -A1 -E '^http:' "$CFG" | grep -E 'port:' | awk '{print $2}')"
    if ! wait_up; then
      echo "harbor: services did not come up - inspect with: cd $DIST && podman ps -a" >&2
      exit 1
    fi
    # Harbor's core needs a while past "container running" before its API answers. Report it, but do
    # not fail on it: the platform tolerates an unreachable registry, and the caller is usually a
    # `make up` that has a control plane to get on with.
    if ! wait_healthy "$host" "${port:-8090}"; then
      echo "harbor: containers are up but the API is not answering yet - give it a minute."
    fi
    cat <<EOF

harbor: up at http://${host}:${port:-8090}

Point the platform at it, in .env:
  KAAS_REGISTRY_HOST=${host}:${port:-8090}
  KAAS_REGISTRY_UI_URL=http://${host}:${port:-8090}

Then \`make up\` - now that harbor.yml exists it carries the registry wiring automatically.

The platform creates its own projects (kaas-library, kaas-cache-*, one per cluster) on the leader's
first sweep; it never touches anything else in this Harbor.
EOF
    ;;
  stop)
    # The gentle counterpart of `ensure`, kept for `make down KEEP_CACHE=1`. Same never-fail-a-
    # teardown contract as `reap`; it just leaves the data behind, so the pull-through cache is still
    # warm on the next `make up`.
    [[ -d "$DIST" ]] || exit 0
    if ! "$0" down; then
      echo "harbor: could not stop it - stop it by hand with 'make harbor-down'" >&2
    fi
    exit 0
    ;;
  reap)
    # The counterpart of `ensure`, and what every `make down` runs, for the same reason: a bring-up
    # switch that is not symmetric is a surprise - and neither is a "full cleanup" that leaves the
    # largest thing on the host behind. Same contract as `ensure`: a quiet no-op where there is no
    # Harbor, and NEVER a reason for a teardown to fail.
    #
    # It gates on the CONFIG, not on the installer directory, because those two go away
    # independently: `deploy/harbor/harbor` is gitignored and re-downloadable, while data_volume is
    # the gigabytes. Gating on the installer would silently skip the data.
    [[ -f "$CFG" ]] || exit 0
    if ! "$0" purge; then
      echo "harbor: could not remove it - do it by hand with 'make harbor-purge'" >&2
    fi
    exit 0
    ;;
  down)
    [[ -d "$DIST" ]] || { echo "harbor: not installed"; exit 0; }
    exclusive "$@"
    # `|| true` for the same reason as `up`: podman-compose reports a nonzero status for containers
    # that were already gone, which is not a failure to stop them.
    (cd "$DIST" && compose -f docker-compose.yml down) || true
    ;;
  purge)
    # Deliberately separate from `down`, and deliberately COMPLETE: `compose down -v` reaches only
    # containers and their volumes, and Harbor declares none - every stateful path is a host bind
    # mount under data_volume, so stopping at the containers is what left a purged Harbor still
    # holding its database. remove_data is the rest of it.
    #
    # It runs on a CONFIGURED host even when the installer directory is gone (see `reap`): the
    # directories named in harbor.yml outlive it, and they are the point.
    need_config
    exclusive "$@"
    if [[ -d "$DIST" ]]; then
      (cd "$DIST" && compose -f docker-compose.yml down -v) || true
    fi
    remove_data
    echo "harbor: purged - every cached and pushed image is gone, and the next 'make up' starts a"
    echo "harbor: registry with an empty database. The goharbor container images are untouched."
    ;;
  *)
    echo "usage: $0 [up|ensure|down|stop|purge|reap]" >&2
    exit 1
    ;;
esac
