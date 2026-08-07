# HashiCorp Vault, configured for the compose deployment (internal/vault).
#
# This replaces `vault server -dev`, whose storage is IN-MEMORY: every restart of the container wiped
# the `kaas` KV mount and every policy the platform had written, while Postgres went on believing the
# work was done (Cluster.VaultWired latches and nothing clears it). The visible symptom was a "View in
# Vault" token that Vault accepted and that granted nothing.
#
# Still a lab deployment, and still a documented shortcut: one unseal share, the unseal key and root
# token written next to the data by entrypoint.sh, no TLS on the listener, no audit device. Production
# would run HA Vault with auto-unseal via a KMS, a real storage backend, TLS, and an audit device.

storage "file" {
  path = "/vault/file"
}

listener "tcp" {
  address     = "0.0.0.0:8200"
  tls_disable = 1
}

# Reachable from the platform's containers as `vault:8200` and published to the host for the UI
# handoff by compose.ports.yaml.
api_addr = "http://vault:8200"

ui = true

# Vault mlocks its memory so secrets cannot be swapped out, and refuses to start when it cannot -
# which is what a container's default memlock rlimit gives you, and why dev mode disabled it silently.
# Turned off here for the same reason HashiCorp's own Vault Helm chart turns it off by default in
# containers. Production would raise the rlimit and leave this on (or disable swap on the host).
disable_mlock = true
