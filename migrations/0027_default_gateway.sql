-- 0027_default_gateway.sql - default north-south ingress: every cluster reserves one node-network
-- address for its default MetalLB L2 pool, from which the default Envoy Gateway draws its external
-- IP. load_balancer_ip is desired state decided once at admission (netpool.LoadBalancerIP on kvm,
-- allocated from the operator range in shared static mode, user-supplied in shared dhcp mode) - the
-- same shape as api_vip. gateway_wired is the observed-state marker recording that the MetalLB pool
-- + Envoy GatewayClass/Gateway CRs have been applied (see reconcileGatewayWiring / the default_gateway
-- ansible role), so the idempotent kubectl apply is skipped once done. Pre-existing rows default to
-- '' / false: their metallb/envoy-gateway add-ons (if any) predate this and the wiring simply never
-- fires until a reserved IP is present.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS load_balancer_ip TEXT    NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS gateway_wired    BOOLEAN NOT NULL DEFAULT false;
