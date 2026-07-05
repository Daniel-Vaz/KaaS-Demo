// Package tofurunner holds the OpenTofu mechanics shared by every OpenTofu-backed provisioner
// (internal/provision/tofu for libvirt/KVM, internal/provision/vsphere for vSphere): one
// workspace per cluster under WorkDir, the module copied into it, variables written as
// terraform.tfvars.json, init/apply/destroy streamed into the cluster's event timeline, and the
// `nodes` output parsed back into provision.ProvisionedNode.
//
// The backend-specific parts stay with each provisioner: which module, which variables, and how
// a golden image is resolved. Postgres remains the single source of truth - a workspace is a
// derived artifact, and `apply` re-converges if it is lost.
package tofurunner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

// Runner executes OpenTofu against one module, in per-cluster workspaces under WorkDir.
type Runner struct {
	Bin       string // "tofu"
	ModuleDir string // abs path to the module (its *.tf files are copied into each workspace)
	WorkDir   string // base dir for per-cluster workspaces
	// ExtraEnv is appended to the process environment of every tofu invocation - where a
	// backend passes provider credentials (e.g. VSPHERE_PASSWORD), so they never land in a
	// tfvars file on disk.
	ExtraEnv []string
	Events   events.Sink  // optional; streams tofu output into the cluster timeline
	Log      *slog.Logger // required
}

// Workspace is the per-cluster directory holding the module copy, tfvars and OpenTofu state.
func (r *Runner) Workspace(clusterID string) string {
	return filepath.Join(r.WorkDir, clusterID)
}

// EnsureWorkspace creates the cluster's workspace and copies the module into it (idempotent).
func (r *Runner) EnsureWorkspace(clusterID string) (string, error) {
	ws := r.Workspace(clusterID)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(r.ModuleDir)
	if err != nil {
		return "", fmt.Errorf("copy module: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.ModuleDir, e.Name()))
		if err != nil {
			return "", fmt.Errorf("copy module: %w", err)
		}
		if err := os.WriteFile(filepath.Join(ws, e.Name()), b, 0o644); err != nil {
			return "", fmt.Errorf("copy module: %w", err)
		}
	}
	return ws, nil
}

// WriteVars marshals the backend's variables into the workspace's terraform.tfvars.json.
func (r *Runner) WriteVars(ws string, vars any) error {
	b, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ws, "terraform.tfvars.json"), b, 0o644)
}

// resetProviderState removes a workspace's DERIVED provider-selection artifacts - the .terraform
// directory (provider plugins linked in from the image's mirror) and .terraform.lock.hcl (the
// pinned provider versions + checksums) - so the next `tofu init` re-resolves them from whatever
// providers the CURRENT image ships. terraform.tfstate and terraform.tfvars.json are left
// untouched: the state is the one irreplaceable file (it maps live infra to resource IDs), the
// image's mirror is the source of truth for provider versions, and the module .tf files are
// re-copied by EnsureWorkspace each run.
//
// This is what lets a long-running deployment take upgrades without wedging existing clusters.
// A persisted lock pins the version an OLD image resolved; ship a new image whose mirror carries a
// newer provider and `tofu init` refuses to reconcile ("Could not resolve provider … the
// previously-selected version is no longer available"), which - because init gates apply AND
// destroy - freezes every future reconcile of that cluster, including its teardown. Regenerating
// the selection from the image each init sidesteps that entirely.
func (r *Runner) resetProviderState(ws string) error {
	for _, name := range []string{".terraform", ".terraform.lock.hcl"} {
		if err := os.RemoveAll(filepath.Join(ws, name)); err != nil {
			return fmt.Errorf("reset provider state: %w", err)
		}
	}
	return nil
}

// Init re-resolves the providers (from the image's mirror) and initialises the workspace. It always
// drops the stale provider selection first (resetProviderState), so init is governed by the image,
// not by a lock file frozen at create time.
func (r *Runner) Init(ctx context.Context, ws, clusterID string) error {
	if err := r.resetProviderState(ws); err != nil {
		return err
	}
	return r.Run(ctx, ws, clusterID, "init", "-input=false", "-no-color")
}

// Apply runs init + apply and returns the module's `nodes` output.
func (r *Runner) Apply(ctx context.Context, ws, clusterID string) ([]provision.ProvisionedNode, error) {
	if err := r.Init(ctx, ws, clusterID); err != nil {
		return nil, fmt.Errorf("tofu init: %w", err)
	}
	if err := r.Run(ctx, ws, clusterID, "apply", "-auto-approve", "-input=false", "-no-color"); err != nil {
		return nil, fmt.Errorf("tofu apply: %w", err)
	}
	return r.OutputNodes(ctx, ws, clusterID)
}

// ApplyReplacing runs init + apply with a `-replace=` for each given resource address, forcing those
// specific resources to be destroyed and re-created even though nothing about the desired state has
// changed. Everything else in the workspace converges normally.
//
// This is what makes node repair expressible at all. An ordinary apply is a diff, and a VM that is
// present but broken diffs clean - so "rebuild this node onto the image it is already running" has
// no declarative expression. `-replace` is OpenTofu's own answer to exactly that: the operator
// asserting that a resource is unhealthy, which the platform is doing here on the operator's behalf.
//
// Addresses that do not exist in state are refused by tofu with a hard error rather than ignored, so
// callers build them from the module's own naming (for_each keyed on VM name) rather than guessing.
func (r *Runner) ApplyReplacing(ctx context.Context, ws, clusterID string, addrs []string) ([]provision.ProvisionedNode, error) {
	if len(addrs) == 0 {
		return r.Apply(ctx, ws, clusterID)
	}
	if err := r.Init(ctx, ws, clusterID); err != nil {
		return nil, fmt.Errorf("tofu init: %w", err)
	}
	args := []string{"apply", "-auto-approve", "-input=false", "-no-color"}
	for _, a := range addrs {
		args = append(args, "-replace="+a)
	}
	if err := r.Run(ctx, ws, clusterID, args...); err != nil {
		return nil, fmt.Errorf("tofu apply -replace: %w", err)
	}
	return r.OutputNodes(ctx, ws, clusterID)
}

// DestroyAndRemove destroys the cluster's infrastructure and drops its workspace. Idempotent:
// a missing workspace means nothing was provisioned.
func (r *Runner) DestroyAndRemove(ctx context.Context, clusterID string) error {
	ws := r.Workspace(clusterID)
	if _, err := os.Stat(ws); os.IsNotExist(err) {
		return nil
	}
	// `tofu destroy` needs the providers just like apply, and after an image upgrade the workspace's
	// linked plugins/lock are stale - so re-init from the current image first, or teardown is the
	// one operation an upgrade could permanently wedge (see resetProviderState).
	if err := r.Init(ctx, ws, clusterID); err != nil {
		return fmt.Errorf("tofu init: %w", err)
	}
	if err := r.Run(ctx, ws, clusterID, "destroy", "-auto-approve", "-input=false", "-no-color"); err != nil {
		return fmt.Errorf("tofu destroy: %w", err)
	}
	return os.RemoveAll(ws)
}

// OutputNodes runs `tofu output -json` and parses the module's `nodes` map - the contract every
// backend module satisfies: { "nodes": { "value": { "<key>": {name, ip, mac}, … } } }.
func (r *Runner) OutputNodes(ctx context.Context, ws, clusterID string) ([]provision.ProvisionedNode, error) {
	out, err := procstream.Capture(ctx, ws, r.env(), r.EmitFor(clusterID), r.bin(), "output", "-json", "-no-color")
	if err != nil {
		return nil, fmt.Errorf("tofu output: %w", err)
	}
	var outputs struct {
		Nodes struct {
			Value map[string]struct {
				Name string `json:"name"`
				IP   string `json:"ip"`
				MAC  string `json:"mac"`
				// Extra disks: logical name → the guest-visible identity token (see
				// provision.ProvisionedNode.Disks). Absent on a node with no extra disks.
				Disks map[string]string `json:"disks"`
			} `json:"value"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(out, &outputs); err != nil {
		return nil, fmt.Errorf("tofu output parse: %w", err)
	}
	nodes := make([]provision.ProvisionedNode, 0, len(outputs.Nodes.Value))
	for _, n := range outputs.Nodes.Value {
		// Drop disks the module reported with an empty identity: on vsphere the VMDK's UUID may not
		// be readable on the tick that created it. Carrying an empty token upward would make the
		// reconciler think it knows the device when it doesn't, so it is simply "not observed yet"
		// and a later tick reports it.
		var disks map[string]string
		for name, id := range n.Disks {
			if id == "" {
				continue
			}
			if disks == nil {
				disks = make(map[string]string, len(n.Disks))
			}
			disks[name] = id
		}
		nodes = append(nodes, provision.ProvisionedNode{VMName: n.Name, IP: n.IP, MAC: n.MAC, Disks: disks})
	}
	return nodes, nil
}

// OutputJSON returns the workspace's raw `tofu output -json`, for a backend that reads an output
// beyond the shared `nodes` contract (the libvirt module's extra_disks - see
// internal/provision/tofu.attachDisks).
func (r *Runner) OutputJSON(ctx context.Context, ws, clusterID string) ([]byte, error) {
	out, err := procstream.Capture(ctx, ws, r.env(), r.EmitFor(clusterID), r.bin(), "output", "-json", "-no-color")
	if err != nil {
		return nil, fmt.Errorf("tofu output: %w", err)
	}
	return out, nil
}

// ListManaged returns the cluster IDs with a workspace under WorkDir - the infra this runner is
// tracking. Each workspace maps 1:1 to a cluster (it holds its OpenTofu state). Production would
// additionally query the hypervisor for VMs carrying our metadata, to catch infra whose workspace
// was lost; here the workspace is the source of truth for GC.
//
// Only actual OpenTofu workspaces count. WorkDir is shared with other components that keep their
// own per-cluster scratch under it (e.g. the shell's WorkDir/shell/<id>/<session> and the kube
// seam's WorkDir/kube/<id>), so a bare "every subdirectory is a cluster" scan would report those
// bookkeeping dirs as orphaned infrastructure and the GC would delete them out from under live
// sessions. A workspace is identified by the module's `.tf` files, copied in at create time and
// present for its whole life; the sibling dirs never contain any. Giving each backend its own
// WorkDir keeps one provisioner from ever seeing another's workspaces.
func (r *Runner) ListManaged(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(r.WorkDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && isWorkspace(filepath.Join(r.WorkDir, e.Name())) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

func isWorkspace(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tf") {
			return true
		}
	}
	return false
}

// Run executes a tofu subcommand in ws, streaming stdout+stderr as events and logs.
func (r *Runner) Run(ctx context.Context, ws, clusterID string, args ...string) error {
	return procstream.Run(ctx, ws, r.env(), r.EmitFor(clusterID), r.bin(), args...)
}

// EmitFor returns a per-cluster line sink that logs and (if configured) emits events.
func (r *Runner) EmitFor(clusterID string) func(string) {
	return func(line string) {
		r.Log.Info("tofu", "cluster", clusterID, "line", line)
		if r.Events != nil {
			r.Events.Emit(events.Event{ClusterID: clusterID, Level: "info", Source: "infra", Message: line})
		}
	}
}

// Emit logs a one-off message and (if configured) publishes it to the cluster's event timeline.
func (r *Runner) Emit(clusterID, level, msg string) {
	r.Log.Info("tofu", "cluster", clusterID, "level", level, "line", msg)
	if r.Events != nil {
		r.Events.Emit(events.Event{ClusterID: clusterID, Level: level, Source: "infra", Message: msg})
	}
}

func (r *Runner) bin() string {
	if r.Bin == "" {
		return "tofu"
	}
	return r.Bin
}

func (r *Runner) env() []string {
	return append(append(os.Environ(), "TF_IN_AUTOMATION=1"), r.ExtraEnv...)
}
