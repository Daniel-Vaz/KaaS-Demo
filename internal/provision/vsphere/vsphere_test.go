package vsphere

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		ModuleDir:      t.TempDir(),
		WorkDir:        t.TempDir(),
		URL:            "https://vcenter.example.internal",
		Username:       `LAB\user`,
		Password:       "s3cret",
		Insecure:       true,
		Datacenter:     "MyDC",
		ComputeCluster: "CLUSTER01",
		Datastore:      "datastorenl",
		ParentFolder:   "DVaz",
		SSHPublicKey:   "ssh-ed25519 AAAA test",
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// vCenter credentials must reach OpenTofu through the process ENVIRONMENT, never through the
// tfvars file - which lives on disk in the workspace for the whole life of the cluster.
func TestCredentialsGoToEnvNotTfvars(t *testing.T) {
	p, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(p.run.ExtraEnv, "\n")
	for _, want := range []string{
		"VSPHERE_SERVER=vcenter.example.internal", // bare host, no scheme
		`VSPHERE_USER=LAB\user`,
		"VSPHERE_PASSWORD=s3cret",
		"VSPHERE_ALLOW_UNVERIFIED_SSL=true",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("tofu env is missing %q; got:\n%s", want, env)
		}
	}
}

func TestNewRequiresConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Datastore = ""
	if _, err := New(cfg); err == nil {
		t.Fatal("New with no datastore = nil error, want a rejection (the clone would have nowhere to land)")
	}
}

// writeVars is exercised through EnsureNodes' failure path: we can't apply without a vCenter, but
// the tfvars written before the apply are the contract with the module, so assert on them.
func writtenVars(t *testing.T, p *Provisioner, clusterID string, net provision.NetworkSpec, specs []provision.NodeSpec) map[string]any {
	t.Helper()
	// tofu isn't run here (the binary is bogus), so EnsureNodes fails - after writing the vars.
	_, _ = p.EnsureNodes(t.Context(), clusterID, net, specs)
	b, err := os.ReadFile(filepath.Join(p.run.WorkDir, clusterID, "terraform.tfvars.json"))
	if err != nil {
		t.Fatalf("tfvars not written: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWriteVarsDHCP(t *testing.T) {
	cfg := testConfig(t)
	cfg.Bin = "/nonexistent/tofu"
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := writtenVars(t, p, "abc123",
		provision.NetworkSpec{
			CIDR: "172.23.252.0/24", Mode: "dhcp", Name: "serviceVMNetwork",
			VIP: "172.23.252.240", ClusterName: "demo",
		},
		[]provision.NodeSpec{{
			VMName: "demo-cp-0", Role: domain.RoleControlPlane, CPUs: 2, MemMB: 4096, DiskGB: 30,
			Image: "ubuntu-24.04-k8s-1.36.2",
		}},
	)

	if got["ip_mode"] != "dhcp" || got["network"] != "serviceVMNetwork" {
		t.Errorf("ip_mode/network = %v/%v, want dhcp/serviceVMNetwork", got["ip_mode"], got["network"])
	}
	// The per-cluster folder carries the cluster's name AND id, so it is identifiable in vCenter.
	if got["folder_name"] != "demo-abc123" || got["parent_folder"] != "DVaz" {
		t.Errorf("folder = %v under %v, want demo-abc123 under DVaz", got["folder_name"], got["parent_folder"])
	}
	nodes := got["nodes"].([]any)
	node := nodes[0].(map[string]any)
	if node["image"] != "ubuntu-24.04-k8s-1.36.2" {
		t.Errorf("node image = %v, want the VM template name", node["image"])
	}
	if node["ip"] != "" {
		t.Errorf("node ip = %v, want empty in dhcp mode (the network's DHCP assigns it)", node["ip"])
	}
	// Credentials must never appear in the file.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), `LAB\user`) {
		t.Fatal("vCenter credentials leaked into terraform.tfvars.json")
	}
}

func TestWriteVarsStatic(t *testing.T) {
	cfg := testConfig(t)
	cfg.Bin = "/nonexistent/tofu"
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := writtenVars(t, p, "abc123",
		provision.NetworkSpec{
			CIDR: "172.23.252.0/24", Mode: "static", Name: "serviceVMNetwork",
			Gateway: "172.23.252.1", DNS: []string{"172.23.252.10"}, ClusterName: "demo",
		},
		[]provision.NodeSpec{{VMName: "demo-cp-0", Role: domain.RoleControlPlane, Image: "ubuntu-24.04-k8s-1.36.2", IP: "172.23.252.50"}},
	)
	if got["ip_mode"] != "static" || got["gateway"] != "172.23.252.1" {
		t.Errorf("ip_mode/gateway = %v/%v, want static/172.23.252.1", got["ip_mode"], got["gateway"])
	}
	node := got["nodes"].([]any)[0].(map[string]any)
	if node["ip"] != "172.23.252.50" {
		t.Errorf("node ip = %v, want the platform's pre-allocated address", node["ip"])
	}
}

// A static-mode node with no allocated address must fail LOUDLY: booting it would put the VM on
// whatever address it could get, which is not the one the control plane recorded.
func TestStaticWithoutIPIsRejected(t *testing.T) {
	p, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.EnsureNodes(t.Context(), "abc",
		provision.NetworkSpec{CIDR: "172.23.252.0/24", Mode: "static", Name: "serviceVMNetwork"},
		[]provision.NodeSpec{{VMName: "demo-cp-0", Image: "img"}},
	)
	if err == nil || !strings.Contains(err.Error(), "no static IP") {
		t.Fatalf("EnsureNodes(static, no IP) error = %v, want a rejection", err)
	}
}

// Each backend keeps its own workspace root, so one provisioner's orphan sweep can never see -
// and destroy - the other's clusters.
func TestListManagedSeesOnlyItsOwnWorkspaces(t *testing.T) {
	cfg := testConfig(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(cfg.WorkDir, "vs-cluster")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.tf"), []byte("# module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A scratch dir that is not a workspace (no .tf) must not be reported as infrastructure.
	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}

	managed, err := p.ListManaged(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(managed, []string{"vs-cluster"}) {
		t.Fatalf("ListManaged = %v, want only [vs-cluster]", managed)
	}
}

func TestSoapURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://vcenter.lan", "https://vcenter.lan/sdk"},
		{"vcenter.lan", "https://vcenter.lan/sdk"},
		{"https://vcenter.lan/", "https://vcenter.lan/sdk"},
	} {
		u, err := soapURL(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if u.String() != tc.want {
			t.Errorf("soapURL(%q) = %q, want %q", tc.in, u, tc.want)
		}
	}
}
