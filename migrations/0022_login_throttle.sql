-- 0022_login_throttle.sql - failed-login counters, so the portal can't be used to lock out the
-- directory.
--
-- Why this exists: POST /auth/login is public and unauthenticated, and with directory auth every
-- failed login becomes a real bad-password bind against a real Active Directory account. AD lockout
-- policies typically trip at 3–5 failures. Without a counter, anyone who can reach the portal could
-- script a name list and lock out every account in the domain - a denial of service against the
-- company, delivered through a control plane that has no business being able to do that.
--
-- It lives in Postgres rather than in process memory because the api tier scales horizontally
-- (`make up-scale API=3`): an in-memory counter would give an attacker one full allowance PER
-- REPLICA and would reset on every deploy. Nothing may be pinned to a replica (see CLAUDE.md).
--
-- Two independent scopes, because they defend different things:
--   'user' - keyed on the username. The load-bearing one: it is what protects a given AD account
--            from being locked, and it holds even when the attacker rotates source addresses.
--   'ip'   - keyed on the client address. Catches one source spraying MANY usernames, which the
--            per-user counter alone would never see.
CREATE TABLE login_failures (
    scope        TEXT NOT NULL,          -- 'user' | 'ip'
    key          TEXT NOT NULL,          -- the username, or the client IP
    failures     INT  NOT NULL DEFAULT 0,
    window_start TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, key)
);

-- Lets the periodic sweep drop expired rows without a full scan; the table is otherwise unbounded
-- in the number of distinct usernames anyone has ever guessed at.
CREATE INDEX login_failures_window ON login_failures (window_start);
