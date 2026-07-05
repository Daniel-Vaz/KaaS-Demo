package ansible

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// TestDiscardControlPlaneBackup checks the snapshot lifecycle fix: purging removes any leftover
// control-plane backup archives so a later OS roll cannot silently reuse a stale etcd snapshot.
func TestDiscardControlPlaneBackup(t *testing.T) {
	work := t.TempDir()
	m, err := New(Config{
		PlaybookDir:       t.TempDir(),
		WorkDir:           work,
		SSHPrivateKeyFile: filepath.Join(t.TempDir(), "id"),
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &domain.Cluster{
		ID: "c1", Name: "demo",
		Nodes: []domain.Node{{ID: "cp0", Role: domain.RoleControlPlane, VMName: "demo-cp-0", IP: "192.168.122.10"}},
	}

	// Seed a stale snapshot in the cluster's artifacts dir.
	art := filepath.Join(work, c.ID, "artifacts")
	if err := os.MkdirAll(art, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"etcd-data.tar.gz", "kube-etc.tar.gz", "kubelet-data.tar.gz"} {
		if err := os.WriteFile(filepath.Join(art, f), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.DiscardControlPlaneBackup(context.Background(), c); err != nil {
		t.Fatalf("discard: %v", err)
	}
	for _, f := range []string{"etcd-data.tar.gz", "kube-etc.tar.gz", "kubelet-data.tar.gz"} {
		if _, err := os.Stat(filepath.Join(art, f)); !os.IsNotExist(err) {
			t.Fatalf("archive %s still present after discard (err=%v)", f, err)
		}
	}

	// Idempotent: a second discard (nothing to remove) must not error.
	if err := m.DiscardControlPlaneBackup(context.Background(), c); err != nil {
		t.Fatalf("second discard should be a no-op: %v", err)
	}
}

// TestJoinControlPlaneReprovisionsLoadBalancerForHA guards against the bug where a rolling HA
// control-plane replacement never re-installed keepalived+haproxy on the replacement node: rolled
// one node at a time, that silently erodes the VIP until, after the LAST control-plane node is
// replaced, no node anywhere can hold it and the whole cluster becomes unreachable. JoinControlPlane
// must pass the same ha/control_plane_vip/vrrp_router_id/vrrp_password values InitControlPlane used,
// so join-controlplane.yml's loadbalancer role (guarded by `when: ha`) actually runs. Uses "true" as
// the ansible-playbook binary so this runs without a real Ansible install; the extravars file is
// written to disk before that binary is even invoked.
func TestJoinControlPlaneReprovisionsLoadBalancerForHA(t *testing.T) {
	work := t.TempDir()
	m, err := New(Config{
		Bin:               "true",
		PlaybookDir:       t.TempDir(),
		WorkDir:           work,
		SSHPrivateKeyFile: filepath.Join(t.TempDir(), "id"),
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &domain.Cluster{
		ID: "c1", Name: "demo", ControlPlanes: 3, APIVIP: "192.168.122.240",
		Nodes: []domain.Node{
			{ID: "cp0", Role: domain.RoleControlPlane, VMName: "demo-cp-0", IP: "192.168.122.10"},
			{ID: "cp1", Role: domain.RoleControlPlane, VMName: "demo-cp-1", IP: "192.168.122.11"},
			{ID: "cp2", Role: domain.RoleControlPlane, VMName: "demo-cp-2", IP: "192.168.122.12"},
		},
	}

	if err := m.JoinControlPlane(context.Background(), c, c.Nodes[2]); err != nil {
		t.Fatalf("JoinControlPlane: %v", err)
	}

	varsPath := filepath.Join(work, c.ID, "extravars-join-controlplane.json")
	b, err := os.ReadFile(varsPath)
	if err != nil {
		t.Fatalf("read extravars: %v", err)
	}
	var extra map[string]any
	if err := json.Unmarshal(b, &extra); err != nil {
		t.Fatal(err)
	}
	if ha, _ := extra["ha"].(bool); !ha {
		t.Fatalf("extravars missing ha=true: %v", extra)
	}
	if extra["control_plane_vip"] != c.APIVIP {
		t.Fatalf("control_plane_vip = %v, want %q", extra["control_plane_vip"], c.APIVIP)
	}
	if _, ok := extra["vrrp_router_id"]; !ok {
		t.Fatalf("extravars missing vrrp_router_id: %v", extra)
	}
	if _, ok := extra["vrrp_password"]; !ok {
		t.Fatalf("extravars missing vrrp_password: %v", extra)
	}
}

// TestJoinWorkersSkipsReadyWaitBeforeCNI guards against a deadlock: join.yml waits for each joined
// worker to report Ready, but a node can never go Ready without a CNI, and the very first
// JoinWorkers call (from PhaseControlPlaneReady) runs BEFORE InstallCNI - every fresh cluster with
// workers would hang for the full wait timeout. JoinWorkers must pass wait_ready=false only for
// that call site; every other phase (CNI already installed) must leave it unset (defaults true).
func TestJoinWorkersSkipsReadyWaitBeforeCNI(t *testing.T) {
	work := t.TempDir()
	m, err := New(Config{
		Bin:               "true",
		PlaybookDir:       t.TempDir(),
		WorkDir:           work,
		SSHPrivateKeyFile: filepath.Join(t.TempDir(), "id"),
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	varsPath := filepath.Join(work, "c1", "extravars-join.json")

	readWaitReady := func(c *domain.Cluster) (value bool, present bool) {
		if err := m.JoinWorkers(context.Background(), c); err != nil {
			t.Fatalf("JoinWorkers: %v", err)
		}
		b, err := os.ReadFile(varsPath)
		if err != nil {
			t.Fatalf("read extravars: %v", err)
		}
		var extra map[string]any
		if err := json.Unmarshal(b, &extra); err != nil {
			t.Fatal(err)
		}
		v, ok := extra["wait_ready"]
		if !ok {
			return false, false
		}
		return v.(bool), true
	}

	base := &domain.Cluster{ID: "c1", Name: "demo", K8sVersion: "1.36.2"}

	base.Phase = domain.PhaseControlPlaneReady
	if v, present := readWaitReady(base); !present || v {
		t.Fatalf("initial bring-up: wait_ready = (present=%v, value=%v), want (true, false)", present, v)
	}

	for _, phase := range []domain.Phase{domain.PhaseUpdating, domain.PhaseUpgrading} {
		base.Phase = phase
		if _, present := readWaitReady(base); present {
			t.Fatalf("phase %s: wait_ready should be omitted (defaults true), but was set", phase)
		}
	}
}

// Each worker carries its node pool as an inventory host var, which the worker role turns into the
// kubelet's --node-labels at registration. Control planes belong to no pool and must carry no such
// var - labelling them would advertise a pool that can't schedule the workloads targeting it.
func TestInventoryCarriesNodePool(t *testing.T) {
	work := t.TempDir()
	m, err := New(Config{
		Bin:               "true",
		PlaybookDir:       t.TempDir(),
		WorkDir:           work,
		SSHPrivateKeyFile: filepath.Join(t.TempDir(), "id"),
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &domain.Cluster{
		ID: "c1", Name: "demo", K8sVersion: "1.36.2",
		Nodes: []domain.Node{
			{VMName: "demo-cp-0", Role: domain.RoleControlPlane, IP: "10.0.0.10"},
			{VMName: "demo-default-0", Role: domain.RoleWorker, Pool: "default", IP: "10.0.0.11"},
			{VMName: "demo-gpu-0", Role: domain.RoleWorker, Pool: "gpu", IP: "10.0.0.12"},
		},
	}
	if _, err := m.prep(c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(work, "c1", "inventory.ini"))
	if err != nil {
		t.Fatal(err)
	}
	inv := string(b)
	for _, want := range []string{
		"demo-default-0 ansible_host=10.0.0.11 nodepool=default",
		"demo-gpu-0 ansible_host=10.0.0.12 nodepool=gpu",
	} {
		if !strings.Contains(inv, want) {
			t.Errorf("inventory missing %q:\n%s", want, inv)
		}
	}
	if strings.Contains(inv, "demo-cp-0 ansible_host=10.0.0.10 nodepool") {
		t.Errorf("control plane carries a nodepool var, but belongs to no pool:\n%s", inv)
	}
}

// The label KEY comes from domain.PoolLabel via extra-vars rather than being hard-coded in the role,
// so the Go constant stays its single source.
func TestJoinWorkersPassesPoolLabelKey(t *testing.T) {
	work := t.TempDir()
	m, err := New(Config{
		Bin:               "true",
		PlaybookDir:       t.TempDir(),
		WorkDir:           work,
		SSHPrivateKeyFile: filepath.Join(t.TempDir(), "id"),
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.JoinWorkers(context.Background(), &domain.Cluster{ID: "c1", Name: "demo", K8sVersion: "1.36.2"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(work, "c1", "extravars-join.json"))
	if err != nil {
		t.Fatal(err)
	}
	var extra map[string]any
	if err := json.Unmarshal(b, &extra); err != nil {
		t.Fatal(err)
	}
	if extra["pool_label"] != domain.PoolLabel {
		t.Fatalf("pool_label = %v, want %q", extra["pool_label"], domain.PoolLabel)
	}
}

// TestCNITimeoutScalesWithNodeCount pins the sizing lesson from the incident that motivated it: the
// CNI's `helm --wait` blocks until Cilium's DaemonSet is Ready on EVERY node, so the budget has to
// grow with the cluster. The old fixed 5m was tightest on exactly the largest builds, and
// overrunning it wedged the release in `pending-install`.
func TestCNITimeoutScalesWithNodeCount(t *testing.T) {
	// A single-node cluster must still get more than the base.
	solo := cniTimeout(&domain.Cluster{Name: "solo", Size: "small", ControlPlanes: 1})
	if solo != (cniTimeoutBase + cniTimeoutPerNode).String() {
		t.Errorf("single-node timeout = %s, want %s", solo, cniTimeoutBase+cniTimeoutPerNode)
	}

	// The shape that actually failed: HA control plane + two pools of two.
	big := &domain.Cluster{
		Name: "np-test", Size: "small", ControlPlanes: 3,
		NodePools: []domain.NodePool{
			{Name: "default", Size: "small", DesiredWorkers: 2},
			{Name: "extra", Size: "medium", DesiredWorkers: 2},
		},
	}
	if n := len(domain.DesiredNodes(big)); n != 7 {
		t.Fatalf("fixture has %d nodes, want the 7 that reproduced the failure", n)
	}
	want := (cniTimeoutBase + 7*cniTimeoutPerNode).String()
	if got := cniTimeout(big); got != want {
		t.Errorf("7-node timeout = %s, want %s", got, want)
	}
	// It must exceed the old hard-coded 5m that this cluster overran.
	if cniTimeoutBase+7*cniTimeoutPerNode <= 5*time.Minute {
		t.Error("7-node timeout does not exceed the old fixed 5m")
	}

	// A pathological cluster is capped, so the wait can never outlive River's job timeout.
	huge := &domain.Cluster{
		Name: "huge", Size: "small", ControlPlanes: 3,
		NodePools: []domain.NodePool{{Name: "p", Size: "small", DesiredWorkers: 500}},
	}
	if got := cniTimeout(huge); got != cniTimeoutMax.String() {
		t.Errorf("huge cluster timeout = %s, want the %s cap", got, cniTimeoutMax)
	}
}

// TestCNIOperatorReplicas checks an HA control plane gets a non-single-point-of-failure operator.
func TestCNIOperatorReplicas(t *testing.T) {
	ha := &domain.Cluster{Name: "ha", Size: "small", ControlPlanes: 3}
	if got := cniOperatorReplicas(ha); got != 2 {
		t.Errorf("HA operator replicas = %d, want 2", got)
	}
	solo := &domain.Cluster{Name: "solo", Size: "small", ControlPlanes: 1}
	if got := cniOperatorReplicas(solo); got != 1 {
		t.Errorf("single-CP operator replicas = %d, want 1", got)
	}
}

// TestReadCertExpiry checks the epoch contract between the renew_certs role (which writes
// artifacts/cert-expiry as Unix-epoch seconds) and readCertExpiry (which parses it).
func TestReadCertExpiry(t *testing.T) {
	art := t.TempDir()
	want := time.Date(2027, 7, 18, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(art, "cert-expiry"),
		[]byte("  1815912000\n"), 0o600); err != nil { // trailing whitespace tolerated
		t.Fatal(err)
	}
	got, err := readCertExpiry(art)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("readCertExpiry = %s, want %s", got, want)
	}

	// A missing/garbage file is an error, not a zero time silently treated as "expired".
	if _, err := readCertExpiry(t.TempDir()); err == nil {
		t.Error("expected error for a missing cert-expiry file")
	}
	bad := t.TempDir()
	_ = os.WriteFile(filepath.Join(bad, "cert-expiry"), []byte("not-a-number"), 0o600)
	if _, err := readCertExpiry(bad); err == nil {
		t.Error("expected error for an unparseable cert-expiry file")
	}
}
