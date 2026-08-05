//go:build js && wasm

// Command demo-wasm is KubeHarbor's control plane compiled to WebAssembly, so the portal can be
// published as a static site (GitHub Pages) with no backend at all.
//
// It is not a mock. It is cmd/api - the same internal/app, the same internal/api routes, the same
// reconciliation loop and state machine - running inside the browser tab against the in-memory store
// and the fake seams that already exist for `make up-fake`. Clusters really are admitted, really
// walk the phases, really converge; the portal talks to them over a shim that dispatches fetch,
// EventSource and WebSocket into this module instead of onto the network (see internal/api/session.go
// and web/portal/src/demo/). What is missing is exactly what the fakes are: OpenTofu, Ansible, Helm,
// a hypervisor, Vault, a directory. Every seam here is fake by construction and cannot be configured
// otherwise, so nothing in this binary can reach anything.
//
// Consequences worth knowing: state is per-tab and dies with the tab (the store is in memory), each
// visitor gets their own private instance, and there is no shared surface to attack.
//
// Build: GOOS=js GOARCH=wasm go build -o kaas-demo.wasm ./cmd/demo-wasm
package main

import (
	"context"
	"log/slog"
	"os"
	"syscall/js"

	"github.com/Daniel-Vaz/KaaS-demo/internal/api"
	"github.com/Daniel-Vaz/KaaS-demo/internal/app"
	"github.com/Daniel-Vaz/KaaS-demo/internal/version"
)

func main() {
	applyDemoEnv()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	log.Info("kubeharbor browser demo", "version", version.String())

	a, err := app.New(log)
	if err != nil {
		fail(log, "init app", err)
		return
	}
	ctx := context.Background()
	if err := a.StartReconciler(ctx); err != nil {
		fail(log, "start reconciler", err)
		return
	}

	srv := api.NewServer(a, log)
	exportBridge(srv.Routes(), &terminals{srv: srv, app: a})

	// Seed before announcing readiness, so the first page the visitor sees already has a fleet on
	// it rather than an empty-state screen that fills in underneath them.
	if err := seed(ctx, a, log); err != nil {
		// A partial seed is still a usable demo - the portal works, there is just less on it - so
		// this is reported and not fatal.
		log.Warn("demo seed incomplete", "err", err)
	}
	announceReady()

	// The bridge is callback-driven; keep the module alive for the life of the page.
	select {}
}

// demoEnv is the deployment this build is: every seam fake, tenancy and the full add-on bundle on,
// and a reconcile loop fast enough that a visitor watching a cluster come up is not left waiting -
// but slow enough that they can still see it walk the state machine, which is the thing worth
// showing. These are defaults, not overrides: an env already set (wasm_exec's `go.env`) wins, so a
// developer can retune without rebuilding.
var demoEnv = map[string]string{
	"KAAS_PROVISIONER":     "fake",
	"KAAS_CONFIG":          "fake",
	"KAAS_ADDONS":          "fake",
	"KAAS_METRICS":         "fake",
	"KAAS_HEALTH":          "fake",
	"KAAS_SHELL":           "fake",
	"KAAS_NODE_SSH":        "fake",
	"KAAS_KUBE":            "fake",
	"KAAS_MONITORING":      "fake",
	"KAAS_SECURITY":        "fake",
	"KAAS_AUDIT":           "fake",
	"KAAS_TUNNEL":          "fake",
	"KAAS_DNS":             "fake",
	"KAAS_VAULT":           "fake",
	"KAAS_AUTH":            "local",
	"KAAS_INFRA_PROVIDERS": "kvm,vsphere,proxmox",
	"KAAS_DNS_BASE_DOMAIN": "kaas.example.internal",
	// The Secrets page's "View in Vault" handoff opens this address. There is no Vault behind the
	// demo, so it is given an explicitly external one: that is what it would be in a real
	// deployment, and it is what lets the shim recognise the link as leaving the platform and
	// explain itself rather than opening a dead tab (see installLinks in src/demo/shim.ts).
	"KAAS_VAULT_ADDR":         "https://vault.kaas.example.internal",
	"KAAS_RECONCILE_INTERVAL": "150ms",
	"KAAS_ADMIN_USERNAME":     DemoAdminUser,
	"KAAS_ADMIN_PASSWORD":     DemoAdminPassword,
	"KAAS_SECRET_KEY":         "kubeharbor-browser-demo",
	// Capacity ceilings, one per infrastructure - there is deliberately no summed platform total,
	// since KVM headroom cannot fund a vSphere VM. Sized so the seeded fleet uses a visible fraction
	// and the Capacity page shows a platform with headroom rather than one at its limit.
	"KAAS_BUDGET_VCPU":            "128",
	"KAAS_BUDGET_MEM_MB":          "524288",
	"KAAS_BUDGET_DISK_GB":         "8192",
	"KAAS_VSPHERE_BUDGET_VCPU":    "64",
	"KAAS_VSPHERE_BUDGET_MEM_MB":  "262144",
	"KAAS_VSPHERE_BUDGET_DISK_GB": "4096",
	"KAAS_PROXMOX_BUDGET_VCPU":    "64",
	"KAAS_PROXMOX_BUDGET_MEM_MB":  "262144",
	"KAAS_PROXMOX_BUDGET_DISK_GB": "4096",

	// The shared-network providers are enabled so the wizard shows the Infrastructure step and a
	// visitor can build on all three. Both are configured in STATIC mode on purpose: in dhcp mode
	// the platform cannot know a free address outside the site's pool, so every create would demand
	// a hand-picked LoadBalancerIP (and an APIVIP for HA) - a real requirement that would read as a
	// broken form here. Static mode lets the platform allocate, so the wizard needs no extra input.
	"KAAS_VSPHERE_NETWORK":     "VM Network",
	"KAAS_VSPHERE_NET_MODE":    "static",
	"KAAS_VSPHERE_NET_CIDR":    "10.60.0.0/22",
	"KAAS_VSPHERE_NET_GATEWAY": "10.60.0.1",
	"KAAS_VSPHERE_NET_DNS":     "10.60.0.10",
	"KAAS_VSPHERE_NET_RANGE":   "10.60.1.10-10.60.3.250",

	"KAAS_PROXMOX_NET_BRIDGE":  "vmbr0",
	"KAAS_PROXMOX_NET_MODE":    "static",
	"KAAS_PROXMOX_NET_CIDR":    "10.70.0.0/22",
	"KAAS_PROXMOX_NET_GATEWAY": "10.70.0.1",
	"KAAS_PROXMOX_NET_DNS":     "10.70.0.10",
	"KAAS_PROXMOX_NET_RANGE":   "10.70.1.10-10.70.3.250",
}

func applyDemoEnv() {
	for k, v := range demoEnv {
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func logLevel() slog.Level {
	if os.Getenv("KAAS_DEMO_DEBUG") != "" {
		return slog.LevelDebug
	}
	// The browser console is the visitor's, not ours: a reconcile loop logging every tick at Info
	// would bury anything they open it to look at.
	return slog.LevelWarn
}

// announceReady tells the page the API is up and seeded. The shim queues nothing before this, so
// the portal simply does not mount until it fires.
func announceReady() { notify("ready", js.Null()) }

func fail(log *slog.Logger, what string, err error) {
	log.Error(what, "err", err)
	notify("error", js.ValueOf(what+": "+err.Error()))
}

// notify invokes the shim's boot callback, installed on globalThis before the module is started.
func notify(state string, detail js.Value) {
	if fn := js.Global().Get("__kaasBoot"); fn.Type() == js.TypeFunction {
		fn.Invoke(state, detail)
	}
}
