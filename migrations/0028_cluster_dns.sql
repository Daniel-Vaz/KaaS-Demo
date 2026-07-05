-- 0028_cluster_dns.sql - per-cluster DNS: every cluster owns a subdomain of the platform's delegated
-- zone ("<name>.kaas.example.internal") and publishes its apps under a wildcard on the address the
-- default Envoy Gateway holds ("*.apps.<name>.kaas.example.internal" A load_balancer_ip). Both names
-- are desired state derived once at admission from deployment config (internal/dns.Settings) and
-- stored here, the same shape as api_vip/load_balancer_ip - so changing KAAS_DNS_BASE_DOMAIN later
-- cannot move an existing cluster's domain out from under its users. dns_wired is the observed-state
-- marker that the wildcard has been published (see reconcileDNSWiring). Pre-existing rows default to
-- '' / false and simply own no name: the wiring never fires without an apps domain.
--
-- No uniqueness index is needed: clusters.name is already globally unique, which is exactly what
-- makes <name>.<base domain> collision-free with no allocator of our own.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS dns_domain  TEXT    NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS apps_domain TEXT    NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS dns_wired   BOOLEAN NOT NULL DEFAULT false;
