// Cluster-derived helpers, mirroring the methods on internal/domain.Cluster so the UI computes
// topology consistently with the control plane.

import type { Cluster, User } from './types';

// canManageCluster mirrors app.CanManageCluster (the server is the real gate): the user may mutate
// this cluster - scale, upgrade, delete, admin kubeconfig - iff they own it, are an admin, or hold
// the Write role in a group they share with the owner. Now that a member's role is per-group, this
// can't be derived from the user alone, so the API stamps each cluster with a server-computed
// `can_manage` flag; we just read it (falling back to the owner/admin check if it's ever absent). A
// read-only member still sees the cluster and gets a read-only kubeconfig + shell (RBAC-limited);
// they just can't act on it. Used to hide/disable write controls, and to pick the admin vs.
// read-only kubeconfig/shell.
export function canManageCluster(c: Cluster, user: User | null | undefined): boolean {
  if (!user) return false;
  return user.is_admin || c.owner_id === user.id || c.can_manage;
}

export function controlPlaneCount(c: Cluster): number {
  return c.control_planes > 0 ? c.control_planes : 1;
}

export function isHA(c: Cluster): boolean {
  return controlPlaneCount(c) > 1;
}

// workerCount mirrors domain.Cluster.WorkerCount(): the total desired workers across every pool.
// Derived rather than served as its own field, because the pools are the single writer of worker
// topology - see internal/domain.
export function workerCount(c: Cluster): number {
  return (c.node_pools ?? []).reduce((n, p) => n + p.desired_workers, 0);
}

export function desiredNodeCount(c: Cluster): number {
  return controlPlaneCount(c) + workerCount(c);
}

// nodesInPool are the provisioned nodes belonging to one pool.
export function nodesInPool(c: Cluster, pool: string): Cluster['nodes'] {
  return (c.nodes ?? []).filter((n) => n.pool === pool);
}

export function provisionedNodeCount(c: Cluster): number {
  return c.nodes?.length ?? 0;
}

export function apiEndpoint(c: Cluster): string {
  return c.api_vip ? `${c.api_vip}:8443` : '';
}

// networkGateway is the address libvirt gives the bridge for a CIDR - the first usable host
// (network address + 1), mirroring internal/netpool.Gateway. Empty for an unparseable/absent CIDR.
export function networkGateway(cidr: string | undefined): string {
  if (!cidr) return '';
  const [addr] = cidr.split('/');
  const parts = addr.split('.').map(Number);
  if (parts.length !== 4 || parts.some((n) => Number.isNaN(n))) return '';
  parts[3] += 1;
  return parts.join('.');
}

// CIDR_RE is a lenient client-side check for a dotted-quad + prefix (the server does the
// authoritative overlap/range validation). It just catches obvious typos before submit.
const CIDR_RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d{1,2})$/;

export function isValidCidr(s: string): boolean {
  const m = CIDR_RE.exec(s.trim());
  if (!m) return false;
  if ([m[1], m[2], m[3], m[4]].map(Number).some((o) => o > 255)) return false;
  const prefix = Number(m[5]);
  return prefix >= 16 && prefix <= 28;
}

// active (non-Deleted) clusters, matching what the old UI listed.
export function activeClusters(list: Cluster[]): Cluster[] {
  return list.filter((c) => c.phase !== 'Deleted');
}

// clusterUsable reports whether the cluster's control plane is up and its kubeconfig valid, so
// Ready-gated UI (resource usage, health, the terminal, kubeconfig download) should stay available.
// Mirrors the reconciler's PhaseReady -> PhaseUpdating -> PhaseReady loop (internal/reconcile
// reconcileOne): an add-on edit or worker scale bumps the generation and moves a Ready cluster
// through Updating without ever tearing down the control plane, unlike initial bring-up (Pending
// through InstallingAddons) where there is no cluster to reach yet.
export function clusterUsable(c: Cluster): boolean {
  return c.phase === 'Ready' || c.phase === 'Updating';
}

// ---- infrastructure providers ----
// Mirrors domain.Cluster.InfraProvider(): a cluster written before multi-provider carries no
// provider and is a KVM cluster.
export function clusterProvider(c: Cluster): string {
  return c.provider || 'kvm';
}

export function providerLabel(provider: string): string {
  switch (provider) {
    case 'vsphere':
      return 'VMware vSphere';
    case 'proxmox':
      return 'Proxmox VE';
    case 'kvm':
      return 'Local KVM';
    default:
      return provider;
  }
}

// ipInSubnet is a lenient client-side check that an address is a usable host in a CIDR - the
// server does the authoritative validation (including collisions with other clusters). It catches
// the common mistake of typing a VIP that isn't on the node network at all.
export function ipInSubnet(ip: string, cidr: string): boolean {
  const octets = (s: string) => {
    const parts = s.trim().split('.').map(Number);
    return parts.length === 4 && parts.every((n) => Number.isInteger(n) && n >= 0 && n <= 255)
      ? parts.reduce((acc, n) => acc * 256 + n, 0)
      : null;
  };
  const [base, prefixRaw] = cidr.split('/');
  const network = octets(base);
  const addr = octets(ip);
  const prefix = Number(prefixRaw);
  if (network === null || addr === null || !Number.isInteger(prefix) || prefix < 1 || prefix > 31) {
    return false;
  }
  const size = 2 ** (32 - prefix);
  const start = Math.floor(network / size) * size;
  // Exclude the network and broadcast addresses - neither is assignable to a host.
  return addr > start && addr < start + size - 1;
}
