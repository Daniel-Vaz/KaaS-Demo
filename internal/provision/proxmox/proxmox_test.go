package proxmox

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
		ModuleDir:    t.TempDir(),
		WorkDir:      t.TempDir(),
		Endpoint:     "https://172.23.234.12:8006/",
		Insecure:     true,
		APIToken:     "kaas@pve!tofu=s3cret-token",
		Node:         "proxmox01",
		Datastore:    "Pool3ParNew",
		Bridge:       "vmbr0",
		SSHPublicKey: "ssh-ed25519 AAAA test",
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Proxmox credentials must reach OpenTofu through the process ENVIRONMENT (PROXMOX_VE_*), never
// through the tfvars file - which lives on disk in the workspace for the whole life of the cluster.
func TestTokenCredentialsGoToEnvNotTfvars(t *testing.T) {
	p, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(p.run.ExtraEnv, "\n")
	for _, want := range []string{
		"PROXMOX_VE_ENDPOINT=https://172.23.234.12:8006/",
		"PROXMOX_VE_INSECURE=true",
		"PROXMOX_VE_API_TOKEN=kaas@pve!tofu=s3cret-token",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("tofu env is missing %q; got:\n%s", want, env)
		}
	}
	if strings.Contains(env, "PROXMOX_VE_USERNAME") {
		t.Error("token auth must not also set PROXMOX_VE_USERNAME")
	}
}

func TestPasswordCredentialsGoToEnv(t *testing.T) {
	cfg := testConfig(t)
	cfg.APIToken = ""
	cfg.Username = "kaas@pve"
	cfg.Password = "s3cret-pass"
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(p.run.ExtraEnv, "\n")
	for _, want := range []string{"PROXMOX_VE_USERNAME=kaas@pve", "PROXMOX_VE_PASSWORD=s3cret-pass"} {
		if !strings.Contains(env, want) {
			t.Errorf("tofu env is missing %q; got:\n%s", want, env)
		}
	}
	if strings.Contains(env, "PROXMOX_VE_API_TOKEN") {
		t.Error("password auth must not also set PROXMOX_VE_API_TOKEN")
	}
}

// Exactly one auth method must be configured - both or neither is a misconfiguration that must fail
// at construction, not at the first apply against Proxmox.
func TestNewRejectsAmbiguousOrMissingAuth(t *testing.T) {
	both := testConfig(t)
	both.Username, both.Password = "kaas@pve", "x" // plus the token from testConfig
	if _, err := New(both); err == nil {
		t.Error("New with BOTH token and username/password = nil error, want a rejection")
	}
	none := testConfig(t)
	none.APIToken = ""
	if _, err := New(none); err == nil {
		t.Error("New with NO auth = nil error, want a rejection")
	}
}

func TestNewRequiresConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Datastore = ""
	if _, err := New(cfg); err == nil {
		t.Fatal("New with no datastore = nil error, want a rejection (the clone would have nowhere to land)")
	}
}

// writtenVars exercises the tfvars written before the (bogus-binary) apply: they are the contract
// with the module, so assert on them.
func writtenVars(t *testing.T, p *Provisioner, clusterID string, net provision.NetworkSpec, specs []provision.NodeSpec) map[string]any {
	t.Helper()
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
			CIDR: "172.23.234.0/24", Mode: "dhcp", Name: "vmbr0",
			VIP: "172.23.234.240", ClusterName: "demo",
		},
		[]provision.NodeSpec{{
			VMName: "demo-cp-0", Role: domain.RoleControlPlane, CPUs: 2, MemMB: 4096, DiskGB: 30,
			Image: "ubuntu-24.04-k8s-1.36.2",
		}},
	)
	if got["ip_mode"] != "dhcp" || got["bridge"] != "vmbr0" || got["node_name"] != "proxmox01" {
		t.Errorf("ip_mode/bridge/node = %v/%v/%v, want dhcp/vmbr0/proxmox01", got["ip_mode"], got["bridge"], got["node_name"])
	}
	if got["datastore"] != "Pool3ParNew" {
		t.Errorf("datastore = %v, want Pool3ParNew", got["datastore"])
	}
	node := got["nodes"].([]any)[0].(map[string]any)
	if node["image"] != "ubuntu-24.04-k8s-1.36.2" {
		t.Errorf("node image = %v, want the template name", node["image"])
	}
	if node["ip"] != "" {
		t.Errorf("node ip = %v, want empty in dhcp mode (the site's DHCP assigns it)", node["ip"])
	}
	// Credentials must never appear in the file.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatal("Proxmox credentials leaked into terraform.tfvars.json")
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
			CIDR: "172.23.234.0/24", Mode: "static", Name: "vmbr0",
			Gateway: "172.23.234.254", DNS: []string{"172.23.234.10"}, ClusterName: "demo",
		},
		[]provision.NodeSpec{{VMName: "demo-cp-0", Role: domain.RoleControlPlane, Image: "img", IP: "172.23.234.50"}},
	)
	if got["ip_mode"] != "static" || got["gateway"] != "172.23.234.254" {
		t.Errorf("ip_mode/gateway = %v/%v, want static/172.23.234.254", got["ip_mode"], got["gateway"])
	}
	node := got["nodes"].([]any)[0].(map[string]any)
	if node["ip"] != "172.23.234.50" {
		t.Errorf("node ip = %v, want the platform's pre-allocated address", node["ip"])
	}
}

// Extra disks are sent in a STABLE name order (the module fixes each disk's SCSI slot from the index),
// and each carries the platform-minted wwn the module turns into the disk serial / by-id identity.
func TestExtraDisksSortedByNameWithWWN(t *testing.T) {
	cfg := testConfig(t)
	cfg.Bin = "/nonexistent/tofu"
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := writtenVars(t, p, "abc123",
		provision.NetworkSpec{CIDR: "172.23.234.0/24", Mode: "dhcp", Name: "vmbr0", ClusterName: "demo"},
		[]provision.NodeSpec{{
			VMName: "demo-default-0", Role: domain.RoleWorker, Image: "img",
			Disks: []provision.DiskSpec{
				{Name: "logs", SizeGB: 20, WWN: "0x5000c50bbbbbbbbb"},
				{Name: "data", SizeGB: 10, WWN: "0x5000c50aaaaaaaaa"},
			},
		}},
	)
	disks := got["nodes"].([]any)[0].(map[string]any)["extra_disks"].([]any)
	if len(disks) != 2 {
		t.Fatalf("got %d disks, want 2", len(disks))
	}
	first := disks[0].(map[string]any)
	if first["name"] != "data" || first["wwn"] != "0x5000c50aaaaaaaaa" {
		t.Errorf("first disk = %v, want data with its wwn (sorted by name)", first)
	}
}

// A static-mode node with no allocated address must fail LOUDLY: booting it would put the VM on
// whatever address DHCP gave it, not the one the control plane recorded.
func TestStaticWithoutIPIsRejected(t *testing.T) {
	p, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.EnsureNodes(t.Context(), "abc",
		provision.NetworkSpec{CIDR: "172.23.234.0/24", Mode: "static", Name: "vmbr0"},
		[]provision.NodeSpec{{VMName: "demo-cp-0", Image: "img"}},
	)
	if err == nil || !strings.Contains(err.Error(), "no static IP") {
		t.Fatalf("EnsureNodes(static, no IP) error = %v, want a rejection", err)
	}
}

// Each backend keeps its own workspace root, so one provisioner's orphan sweep can never see - and
// destroy - another's clusters.
func TestListManagedSeesOnlyItsOwnWorkspaces(t *testing.T) {
	cfg := testConfig(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(cfg.WorkDir, "px-cluster")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.tf"), []byte("# module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	managed, err := p.ListManaged(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(managed, []string{"px-cluster"}) {
		t.Fatalf("ListManaged = %v, want only [px-cluster]", managed)
	}
}
