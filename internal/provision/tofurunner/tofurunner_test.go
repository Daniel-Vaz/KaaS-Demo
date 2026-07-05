package tofurunner

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// resetProviderState must drop only the DERIVED provider-selection artifacts (.terraform and
// .terraform.lock.hcl) so the next init re-resolves against the current image's mirror, while
// leaving the irreplaceable / re-derivable-elsewhere files alone: terraform.tfstate (maps live
// infra to resource IDs), terraform.tfvars.json, and the module .tf files. This is the fix that
// keeps a long-running deployment from wedging on an image upgrade - see the doc comment.
func TestResetProviderStateKeepsStateDropsSelection(t *testing.T) {
	ws := t.TempDir()
	keep := map[string]string{
		"terraform.tfstate":     `{"version":4,"serial":7}`,
		"terraform.tfvars.json": `{"cluster_id":"c1"}`,
		"main.tf":               "terraform {}\n",
	}
	drop := map[string]string{
		".terraform.lock.hcl":                              "provider \"x\" { version = \"0.8.1\" }\n",
		".terraform/providers/reg/dmacvicar/libvirt/x/bin": "plugin",
	}
	for name, body := range keep {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range drop {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := &Runner{Log: slog.Default()}
	if err := r.resetProviderState(ws); err != nil {
		t.Fatalf("resetProviderState: %v", err)
	}

	for name, want := range keep {
		got, err := os.ReadFile(filepath.Join(ws, name))
		if err != nil {
			t.Fatalf("expected %s preserved, got error: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s mutated: got %q want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, ".terraform.lock.hcl")); !os.IsNotExist(err) {
		t.Errorf(".terraform.lock.hcl should have been removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".terraform")); !os.IsNotExist(err) {
		t.Errorf(".terraform should have been removed (err=%v)", err)
	}
}

// A clean workspace (no provider selection yet) must not be an error - the first apply of a new
// cluster resets before its very first init.
func TestResetProviderStateNoopOnCleanWorkspace(t *testing.T) {
	r := &Runner{Log: slog.Default()}
	if err := r.resetProviderState(t.TempDir()); err != nil {
		t.Fatalf("resetProviderState on clean workspace: %v", err)
	}
}
