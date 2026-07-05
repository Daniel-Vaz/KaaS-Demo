-- Quota becomes per-infrastructure.
--
-- A single pooled (quota_vcpu, quota_mem_mb) grant made sense when every cluster landed on the one
-- KVM host. With more than one infrastructure it admits clusters against capacity that cannot host
-- them: a tenant's spare cores on the KVM host are not cores in vCenter, and vice versa. So the
-- grant is now a map of provider -> {vcpu, mem_mb}, and the conserved-pool invariant (non-admin
-- grants sum to at most the ceiling) is enforced once per backend.
--
-- Every existing grant was, by construction, a grant on KVM - that was the only provider - so it
-- migrates there verbatim and no tenant loses or gains capacity.

ALTER TABLE users ADD COLUMN IF NOT EXISTS quotas JSONB NOT NULL DEFAULT '{}'::jsonb;

UPDATE users
SET quotas = jsonb_build_object(
        'kvm', jsonb_build_object('vcpu', quota_vcpu, 'mem_mb', quota_mem_mb))
WHERE quotas = '{}'::jsonb
  AND (quota_vcpu > 0 OR quota_mem_mb > 0);

ALTER TABLE users DROP COLUMN IF EXISTS quota_vcpu;
ALTER TABLE users DROP COLUMN IF EXISTS quota_mem_mb;
