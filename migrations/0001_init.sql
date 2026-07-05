-- 0001_init.sql - initial schema for the Postgres store.
-- Mirrors internal/domain + docs/architecture.md. The in-memory store embeds nodes/addons on
-- the cluster; here they are proper tables. IDs are opaque app-generated strings (not UUIDs),
-- so id/cluster_id columns are TEXT. Applied by the built-in migrator in internal/store/postgres.

CREATE TABLE clusters (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    size                TEXT NOT NULL,
    desired_workers     INT  NOT NULL DEFAULT 0,
    pod_cidr            TEXT NOT NULL,
    svc_cidr            TEXT NOT NULL,
    -- version provenance, resolved from a release bundle at create time (ADR-011)
    bundle              TEXT NOT NULL DEFAULT '',
    os_image            TEXT NOT NULL DEFAULT '',
    k8s_version         TEXT NOT NULL,
    cni                 TEXT NOT NULL,
    phase               TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT '',
    generation          BIGINT NOT NULL DEFAULT 1,
    observed_generation BIGINT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ
);

CREATE TABLE nodes (
    id           TEXT NOT NULL,
    cluster_id   TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    role         TEXT NOT NULL,          -- control-plane | worker
    vm_name      TEXT NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    mac          TEXT NOT NULL DEFAULT '',
    phase        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (cluster_id, vm_name)
);

CREATE TABLE cluster_addons (
    cluster_id  TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    version     TEXT NOT NULL,
    phase       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (cluster_id, name)
);

CREATE TABLE secrets (
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,           -- kubeconfig | join_token | ssh_key
    ciphertext BYTEA NOT NULL,          -- AES-256-GCM (app-level; see ADR-010)
    PRIMARY KEY (cluster_id, kind)
);

CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
    level      TEXT NOT NULL,           -- info | warn | error
    source     TEXT,                    -- infra | ansible | addon | reconciler
    message    TEXT NOT NULL
);
CREATE INDEX events_cluster_ts ON events (cluster_id, ts);

-- Reconciler scans for clusters that still need work (non-terminal phase).
CREATE INDEX clusters_needing_work ON clusters (phase)
    WHERE phase NOT IN ('Deleted', 'Failed');

-- River (the job queue) manages its own tables via its migrator (added in the next milestone).
