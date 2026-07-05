package netbox

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

// Provisioner decorates another provision.Provisioner (a shared-network backend - vSphere or
// Proxmox) so that the addresses a cluster occupies are recorded in NetBox as they are learned, and
// released when the cluster is destroyed. The reconcile loop knows nothing about any of this - it is the same
// Provisioner seam, and registration rides on the step that already discovers the IPs.
//
// Registration failures FAIL the reconcile step. That is deliberate: the loop is level-triggered
// and every step is retried, and both the upsert and the delete are idempotent, so failing is
// safe and guarantees NetBox eventually converges. Swallowing the error would instead skip
// registration permanently - the phase advances, nothing re-triggers it, and the IPAM silently
// drifts from reality, which on a shared subnet is exactly the failure that hands the same
// address to two machines. Turning the integration off (unsetting KAAS_NETBOX_URL) is the
// escape hatch, not a partial write.
type Provisioner struct {
	Inner  provision.Provisioner
	Client *Client
	Events events.Sink
	Log    *slog.Logger
}

// Wrap returns inner decorated with NetBox registration. It implements provision.Unwrapper, so the
// reconciler resolves EVERY optional capability the inner backend has - ImageChecker, NodeReplacer,
// NodeReplacePreflighter, NodePowerer - straight through the decorator (see provision.As*).
//
// This is deliberately NOT done by re-declaring each capability on the wrapper: that is a
// combinatorial set of variants, and the last time it was attempted only ImageChecker was covered,
// which silently stripped NodeReplacer/NodeReplacePreflighter from a NetBox-wrapped vSphere backend
// and left an auto-repaired node drained but never rebuilt. Unwrap is one method and cannot fall out
// of sync with a capability added later. The decorator adds no capability of its own - it only
// records IPs on the calls that discover them - so seeing through it to the inner is always correct.
func Wrap(inner provision.Provisioner, client *Client, sink events.Sink, log *slog.Logger) provision.Provisioner {
	return &Provisioner{Inner: inner, Client: client, Events: sink, Log: log}
}

// Unwrap exposes the wrapped backend so provision.As* can reach its optional capabilities.
func (p *Provisioner) Unwrap() provision.Provisioner { return p.Inner }

func (p *Provisioner) EnsureNodes(ctx context.Context, clusterID string, netSpec provision.NetworkSpec, specs []provision.NodeSpec) ([]provision.ProvisionedNode, error) {
	nodes, err := p.Inner.EnsureNodes(ctx, clusterID, netSpec, specs)
	if err != nil {
		return nil, err
	}
	prefix, err := prefixLen(netSpec.CIDR)
	if err != nil {
		return nil, err
	}
	desc := Description(netSpec.ClusterName, clusterID)
	for _, n := range nodes {
		if n.IP == "" {
			continue // not yet observed; a later tick registers it
		}
		if err := p.Client.EnsureIP(ctx, IPRecord{
			Address:     n.IP + "/" + prefix,
			DNSName:     n.VMName,
			Description: desc,
		}); err != nil {
			p.emit(clusterID, "error", fmt.Sprintf("failed to register %s (%s) in NetBox: %v - will retry", n.IP, n.VMName, err))
			return nil, err
		}
	}
	if netSpec.VIP != "" {
		if err := p.Client.EnsureIP(ctx, IPRecord{
			Address:     netSpec.VIP + "/" + prefix,
			DNSName:     netSpec.ClusterName + "-cp-vip",
			Description: desc,
			Role:        "vip",
		}); err != nil {
			p.emit(clusterID, "error", fmt.Sprintf("failed to register the control-plane VIP %s in NetBox: %v - will retry", netSpec.VIP, err))
			return nil, err
		}
	}
	// The default MetalLB pool / Envoy Gateway address is a real, exclusive host on the shared subnet,
	// so it must be recorded like the node IPs and VIP - an unrecorded address is one the IPAM will
	// hand out twice. NetBox's role enum has no dedicated LoadBalancer entry, so it borrows "vip" (the
	// closest fit - a floating address fronting the cluster, same as the control-plane VIP).
	if netSpec.LoadBalancerIP != "" {
		if err := p.Client.EnsureIP(ctx, IPRecord{
			Address:     netSpec.LoadBalancerIP + "/" + prefix,
			DNSName:     netSpec.ClusterName + "-gateway-lb",
			Description: desc,
			Role:        "vip",
		}); err != nil {
			p.emit(clusterID, "error", fmt.Sprintf("failed to register the LoadBalancer IP %s in NetBox: %v - will retry", netSpec.LoadBalancerIP, err))
			return nil, err
		}
	}
	p.emit(clusterID, "info", fmt.Sprintf("registered %d address(es) in NetBox", len(nodes)+boolToInt(netSpec.VIP != "")+boolToInt(netSpec.LoadBalancerIP != "")))
	return nodes, nil
}

// DestroyCluster releases the cluster's addresses AFTER its VMs are gone - never before, so a
// failed destroy can't leave live machines holding addresses NetBox believes are free.
func (p *Provisioner) DestroyCluster(ctx context.Context, clusterID string) error {
	if err := p.Inner.DestroyCluster(ctx, clusterID); err != nil {
		return err
	}
	if err := p.Client.DeleteCluster(ctx, clusterID); err != nil {
		p.emit(clusterID, "error", fmt.Sprintf("failed to release addresses in NetBox: %v - will retry", err))
		return err
	}
	p.emit(clusterID, "info", "released the cluster's addresses in NetBox")
	return nil
}

func (p *Provisioner) ListManaged(ctx context.Context) ([]string, error) {
	return p.Inner.ListManaged(ctx)
}

func (p *Provisioner) emit(clusterID, level, msg string) {
	p.Log.Info("netbox", "cluster", clusterID, "level", level, "msg", msg)
	if p.Events != nil {
		p.Events.Emit(events.Event{ClusterID: clusterID, Level: level, Source: "netbox", Message: msg})
	}
}

// prefixLen is the subnet's mask length - NetBox stores an address WITH its prefix, so it can
// place it under the right prefix record.
func prefixLen(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return "", fmt.Errorf("netbox: cluster network %q: %w", cidr, err)
	}
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprint(ones), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
