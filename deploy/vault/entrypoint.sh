#!/bin/sh
# Start Vault on persistent storage and make it usable unattended.
#
# `vault server -dev` auto-unsealed itself and handed out a fixed root token, which is what made
# `make up` a single command - but dev mode's storage is in-memory, so a restart silently destroyed
# every policy the platform had written (see vault.hcl). A file backend keeps the state and costs an
# init/unseal cycle, which this script performs: initialise on first boot, unseal on every boot, and
# mint a token whose ID is the fixed KAAS_VAULT_TOKEN the rest of the deployment is configured with,
# so the platform's configuration does not change and no operator has to copy a generated secret.
#
# The unseal key and the initial root token are written to $INIT_FILE inside the data volume. That is
# a deliberate lab shortcut and is no weaker than the fixed dev root token it replaces: anything that
# can read the volume could already read Vault's data. Production would use KMS auto-unseal and never
# materialise an unseal key at all.
set -eu

: "${KAAS_VAULT_TOKEN:=kaas-root-dev}"
VAULT_CONFIG=/vault/config/vault.hcl
INIT_FILE=/vault/file/init.json
export VAULT_ADDR=http://127.0.0.1:8200

vault server -config="$VAULT_CONFIG" &
server_pid=$!
# Forward a stop signal to Vault so the container shuts it down cleanly (a file-backed Vault that is
# SIGKILLed can leave a partial write behind).
trap 'kill -TERM "$server_pid" 2>/dev/null || true' TERM INT

# `vault status` exits non-zero when sealed, so match on the output rather than the exit code. It
# prints Initialized/Sealed as soon as the listener is up, whatever the seal state.
status() { vault status 2>/dev/null || true; }
until status | grep -q '^Initialized'; do
  sleep 1
done

if ! status | grep -qE '^Initialized[[:space:]]+true'; then
  echo "vault-init: initialising (1 share, threshold 1)"
  vault operator init -key-shares=1 -key-threshold=1 -format=json > "$INIT_FILE"
  chmod 600 "$INIT_FILE"
fi

# No jq in this image. `operator init -format=json` pretty-prints, so the key and its array bracket
# land on different lines - flatten first (neither a base64 unseal key nor a token contains
# whitespace, so this cannot corrupt either value) and match on the one long line.
flat=$(tr -d ' \t\n' < "$INIT_FILE")
unseal_key=$(printf '%s' "$flat" | sed -n 's/.*"unseal_keys_b64":\["\([^"]*\)".*/\1/p')
root_token=$(printf '%s' "$flat" | sed -n 's/.*"root_token":"\([^"]*\)".*/\1/p')
if [ -z "$unseal_key" ] || [ -z "$root_token" ]; then
  echo "vault-init: could not read the unseal key or root token from $INIT_FILE" >&2
  exit 1
fi

if status | grep -qE '^Sealed[[:space:]]+true'; then
  echo "vault-init: unsealing"
  vault operator unseal "$unseal_key" >/dev/null
fi

# The stable token the platform authenticates with. Minting it by ID is what keeps KAAS_VAULT_TOKEN a
# fixed value across the init that generates a random root token. Idempotent: on later boots the
# token is already in storage, so this is a lookup and nothing else.
if ! VAULT_TOKEN="$KAAS_VAULT_TOKEN" vault token lookup >/dev/null 2>&1; then
  echo "vault-init: minting the platform token"
  VAULT_TOKEN="$root_token" vault token create \
    -id="$KAAS_VAULT_TOKEN" -policy=root -orphan -ttl=0 -display-name=kaas-platform >/dev/null
fi

echo "vault-init: ready"
wait "$server_pid"
