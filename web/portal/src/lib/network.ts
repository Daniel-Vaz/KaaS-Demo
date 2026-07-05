// Presentation helpers for the Networking page: Service-type colouring, port rendering, and the
// wording for the two ways an app reaches the outside world.

import type { ServicePort, ServiceSummary, GatewaySummary, RouteSummary } from './types';

// serviceTypeColor maps a Service type to a Mantine colour. LoadBalancer is highlighted because it is
// the only type that holds an address reachable from outside the cluster - on this platform, the one
// MetalLB hands out - and that is what the page is about.
export function serviceTypeColor(type: string): string {
  switch (type) {
    case 'LoadBalancer':
      return 'blue';
    case 'NodePort':
      return 'cyan';
    case 'ExternalName':
      return 'grape';
    default:
      return 'gray';
  }
}

// SERVICE_TYPE_LABELS spells out what each Service type means, for tooltips.
export const SERVICE_TYPE_LABELS: Record<string, string> = {
  ClusterIP: 'ClusterIP - reachable only from inside the cluster',
  NodePort: 'NodePort - also reachable on a fixed port of every node',
  LoadBalancer: 'LoadBalancer - assigned an external address (MetalLB, on this platform)',
  ExternalName: 'ExternalName - a CNAME alias to a name outside the cluster',
};

export function serviceTypeLabel(type: string): string {
  return SERVICE_TYPE_LABELS[type] ?? type;
}

// formatPort renders one Service port the way `kubectl get svc` does ("80:31080/TCP", "443/TCP").
export function formatPort(p: ServicePort): string {
  const proto = p.protocol || 'TCP';
  return p.node_port ? `${p.port}:${p.node_port}/${proto}` : `${p.port}/${proto}`;
}

// formatPorts renders a Service's ports as one cell, truncating past three so a Service with a dozen
// ports doesn't blow out the column.
export function formatPorts(ports: ServicePort[] | null | undefined): string {
  const ps = ports ?? [];
  if (ps.length === 0) return '-';
  const shown = ps.slice(0, 3).map(formatPort).join(', ');
  return ps.length > 3 ? `${shown} +${ps.length - 3}` : shown;
}

// externalAddress returns the first external address a Service holds, or "" for a purely internal
// one - the "is this reachable from outside" answer in a single string.
export function externalAddress(s: ServiceSummary): string {
  return (s.external_ips ?? [])[0] ?? '';
}

// healthColor is the shared green/yellow/red for "is this object doing its job": a programmed
// Gateway or an accepted route is green, one still settling is yellow (the controller may simply not
// have reconciled it yet), and there is no red - a Gateway API object that is not accepted reports a
// reason, which the UI shows verbatim rather than grading.
export function readyColor(ok: boolean): string {
  return ok ? 'green' : 'yellow';
}

// gatewayStatusText is the short status word for a Gateway row, with its reason where it has one.
export function gatewayStatusText(g: GatewaySummary): string {
  if (g.programmed) return 'Programmed';
  return g.status ? `Not programmed - ${g.status}` : 'Not programmed';
}

// routeStatusText is the same for a route: accepted by at least one of its parents, or the first
// parent's reason for refusing it (a mismatched hostname is the common one).
export function routeStatusText(r: RouteSummary): string {
  if (r.accepted) return 'Accepted';
  return r.status ? `Not accepted - ${r.status}` : 'Not accepted';
}

// routeKindLabel renders a wire route kind as its Kubernetes Kind.
export function routeKindLabel(kind: string): string {
  return kind.replace(/^(http|grpc|tcp|tls|udp)route$/, (_, p: string) => `${p.toUpperCase()}Route`);
}

// backendsText renders a route's backends as "namespace/name:port" strings, flattened across rules -
// the "what actually serves this" column.
export function backendsText(r: RouteSummary): string[] {
  const out: string[] = [];
  for (const rule of r.rules ?? []) {
    for (const b of rule.backends ?? []) {
      out.push(b.port ? `${b.name}:${b.port}` : b.name);
    }
  }
  return out;
}
