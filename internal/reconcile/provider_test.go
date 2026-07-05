package reconcile

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

// A cluster must be provisioned through the backend its Provider names - the whole point of
// recording it. Two distinct fakes stand in for two real backends: if dispatch is wrong, a
// cluster's VMs land in the other provider's infrastructure.
func TestProvisionerDispatchPerCluster(t *testing.T) {
	r, st := newTestReconciler(t)
	kvmProv, vsphereProv := provision.NewFake(), provision.NewFake()
	r.Prov = kvmProv
	r.Provs = map[string]provision.Provisioner{
		domain.ProviderKVM:     kvmProv,
		domain.ProviderVSphere: vsphereProv,
	}

	clusters := []*domain.Cluster{
		{
			ID: "kvm1", Name: "on-kvm", Provider: domain.ProviderKVM, K8sVersion: "1.36.2", Size: "small",
			CNI: "cilium", Phase: domain.PhasePending, Generation: 1, NetworkCIDR: "10.200.1.0/24",
		},
		{
			ID: "vs1", Name: "on-vsphere", Provider: domain.ProviderVSphere, K8sVersion: "1.36.2", Size: "small",
			CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
			NetworkCIDR: "172.23.252.0/24", NetworkName: "serviceVMNetwork", IPMode: "dhcp",
		},
	}
	for _, c := range clusters {
		if err := st.CreateCluster(c); err != nil {
			t.Fatal(err)
		}
		converge(t, r, st, c.ID)
	}

	managedBy := func(p provision.Provisioner) []string {
		ids, err := p.ListManaged(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		return ids
	}
	if got := managedBy(kvmProv); !slices.Equal(got, []string{"kvm1"}) {
		t.Errorf("kvm provisioner manages %v, want only [kvm1]", got)
	}
	if got := managedBy(vsphereProv); !slices.Equal(got, []string{"vs1"}) {
		t.Errorf("vsphere provisioner manages %v, want only [vs1]", got)
	}

	// A legacy row (no provider) is a KVM cluster, and must still reach a provisioner.
	if r.prov(&domain.Cluster{ID: "old"}) != kvmProv {
		t.Error("a cluster with no provider must dispatch to kvm (the default)")
	}
}

// The vSphere node spec must carry the platform's pre-allocated address (static mode) and the
// provider's image NAME (a template, not a qcow2) - the two things that differ per provider and
// that a node cannot boot correctly without.
func TestNodeSpecsVSphere(t *testing.T) {
	r, _ := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "vs1", Name: "demo", Provider: domain.ProviderVSphere, Size: "small",
		OSImage: "ubuntu-24.04", K8sVersion: "1.36.2", ControlPlanes: 1, NodePools: pool(1),
		IPMode: "static",
		StaticIPs: map[string]string{
			"demo-cp-0": "172.23.252.50",
			"demo-w-0":  "172.23.252.51",
		},
	}
	specs := r.nodeSpecs(c)
	if len(specs) != 2 {
		t.Fatalf("nodeSpecs = %d, want 2", len(specs))
	}
	for _, s := range specs {
		if s.Image != "ubuntu-24.04-k8s-1.36.2" {
			t.Errorf("node %s image = %q, want the vSphere template name ubuntu-24.04-k8s-1.36.2", s.VMName, s.Image)
		}
		if want := c.StaticIPs[s.VMName]; s.IP != want {
			t.Errorf("node %s IP = %q, want the allocated %q", s.VMName, s.IP, want)
		}
	}

	// A KVM cluster keeps the qcow2 name and DHCP (no pre-allocated IP).
	kvm := &domain.Cluster{
		ID: "k1", Name: "demo", Size: "small", OSImage: "ubuntu-24.04", K8sVersion: "1.36.2",
		ControlPlanes: 1,
	}
	ks := r.nodeSpecs(kvm)
	if ks[0].Image != "ubuntu-24.04-k8s-1.36.2.qcow2" {
		t.Errorf("kvm node image = %q, want ubuntu-24.04-k8s-1.36.2.qcow2", ks[0].Image)
	}
	if ks[0].IP != "" {
		t.Errorf("kvm node IP = %q, want empty (its network's DHCP assigns it)", ks[0].IP)
	}
}

// netSpec must hand the provisioner the cluster's OWN recorded network - not the deployment's
// current env - so a re-provision can never land a cluster on a different network than the one it
// was admitted onto.
func TestNetSpecFromClusterRow(t *testing.T) {
	c := &domain.Cluster{
		Name: "demo", Provider: domain.ProviderVSphere, NetworkCIDR: "172.23.252.0/24",
		NetworkName: "serviceVMNetwork", IPMode: "static", NetGateway: "172.23.252.1",
		NetDNS: "172.23.252.10, 172.23.252.11", APIVIP: "172.23.252.60", LoadBalancerIP: "172.23.252.61",
	}
	got := netSpec(c)
	if got.Name != "serviceVMNetwork" || got.Mode != "static" || got.Gateway != "172.23.252.1" {
		t.Fatalf("netSpec = %+v, want the cluster's portgroup/mode/gateway", got)
	}
	if !slices.Equal(got.DNS, []string{"172.23.252.10", "172.23.252.11"}) {
		t.Errorf("netSpec DNS = %v, want the two resolvers split and trimmed", got.DNS)
	}
	if got.VIP != "172.23.252.60" || got.ClusterName != "demo" {
		t.Errorf("netSpec VIP/ClusterName = %q/%q, want them passed through (the NetBox decorator needs them)", got.VIP, got.ClusterName)
	}
	if got.LoadBalancerIP != "172.23.252.61" {
		t.Errorf("netSpec LoadBalancerIP = %q, want it passed through (the NetBox decorator registers it)", got.LoadBalancerIP)
	}
	if kvm := netSpec(&domain.Cluster{Name: "k", NetworkCIDR: "10.200.1.0/24"}); kvm.Mode != "nat" {
		t.Errorf("kvm netSpec mode = %q, want nat", kvm.Mode)
	}
}

// Orphaned infrastructure is per backend, and the cluster row that would say which backend built
// it is exactly what's gone - so GC must sweep every provisioner, not just the default one.
func TestGCSweepsEveryProvisioner(t *testing.T) {
	r, _ := newTestReconciler(t)
	kvmProv, vsphereProv := provision.NewFake(), provision.NewFake()
	r.Prov = kvmProv
	r.Provs = map[string]provision.Provisioner{
		domain.ProviderKVM:     kvmProv,
		domain.ProviderVSphere: vsphereProv,
	}

	ctx := context.Background()
	// Infra with no cluster row behind it, on the NON-default provisioner.
	if _, err := vsphereProv.EnsureNodes(ctx, "orphan", provision.NetworkSpec{CIDR: "172.23.252.0/24"},
		[]provision.NodeSpec{{VMName: "orphan-cp-0"}}); err != nil {
		t.Fatal(err)
	}

	r.GC(ctx)

	managed, err := vsphereProv.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 0 {
		t.Fatalf("vsphere provisioner still manages %v after GC - the orphan sweep skipped it", managed)
	}
}

// In fake mode every provider name maps to ONE shared fake. The sweep must not then process it
// once per name (it would try to destroy the same orphans repeatedly).
func TestGCDedupesSharedProvisioner(t *testing.T) {
	r, _ := newTestReconciler(t)
	shared := provision.NewFake()
	r.Prov = shared
	r.Provs = map[string]provision.Provisioner{
		domain.ProviderKVM:     shared,
		domain.ProviderVSphere: shared,
	}
	if got := len(r.provisioners()); got != 1 {
		t.Fatalf("provisioners() = %d distinct, want 1 (the same fake behind both names)", got)
	}
}

// The rolling OS replacement preflights ImageAvailable on the cluster's OWN provisioner. If it
// asked the default one instead, a vSphere cluster's node could be drained on the strength of a
// KVM image check - and never rebuilt.
func TestUpgradePreflightUsesClusterProvisioner(t *testing.T) {
	r, st := newTestReconcilerWithCatalog(t, upgradeChainCatalog(t))
	kvmProv := provision.NewFake()
	vsphereProv := &recordingImageChecker{Provisioner: provision.NewFake()}
	r.Prov = kvmProv
	r.Provs = map[string]provision.Provisioner{
		domain.ProviderKVM:     kvmProv,
		domain.ProviderVSphere: vsphereProv,
	}

	c := &domain.Cluster{
		ID: "vs1", Name: "demo", Provider: domain.ProviderVSphere, Size: "small",
		OSImage: "ubuntu-22.04", K8sVersion: "1.36.2", CNI: "cilium", CNIVersion: "1.19.5",
		Bundle: "2025.3", TargetBundle: "2026.1", ControlPlanes: 1,
		NetworkCIDR: "172.23.252.0/24", NetworkName: "serviceVMNetwork", IPMode: "dhcp",
		Phase: domain.PhaseUpgrading, Generation: 2,
		Nodes: []domain.Node{{
			VMName: "demo-cp-0", Role: domain.RoleControlPlane, IP: "172.23.252.50",
			Image: catalog.GoldenImageNameFor(domain.ProviderVSphere, "ubuntu-22.04", "1.36.2"),
		}},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	err := r.reconcileOne(context.Background(), c)
	if err == nil {
		t.Fatal("reconcile with a missing vSphere template = nil error, want the preflight to abort the roll")
	}
	if !vsphereProv.checked {
		t.Error("the preflight consulted a provisioner other than the cluster's own")
	}
}

// recordingImageChecker is a provisioner whose golden image is never available (the "template not
// built yet" case the preflight exists to catch), and which records that it was the one asked.
type recordingImageChecker struct {
	provision.Provisioner
	checked bool
}

func (p *recordingImageChecker) ImageAvailable(name string) error {
	p.checked = true
	return fmt.Errorf("golden image (VM template) %q not found", name)
}
