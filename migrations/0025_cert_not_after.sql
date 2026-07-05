-- 0025_cert_not_after.sql - observed-state marker for automatic certificate rotation: the earliest
-- expiry across the cluster's kubeadm-managed control-plane certificates. The reconciler renews when
-- this falls within the renewal window (KAAS_CERT_RENEW_WINDOW). NULL means "not observed yet":
-- fresh clusters stamp it at bring-up, and existing rows (default NULL) are observed once on their
-- next Ready tick, then renewed only if actually within the window.
-- See internal/domain.Cluster.CertNotAfter and internal/reconcile (PhaseRenewingCerts).
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS cert_not_after TIMESTAMPTZ;
