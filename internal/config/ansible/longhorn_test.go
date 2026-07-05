package ansible

import (
	"encoding/json"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// The patch is a MERGE patch keyed by disk name, which is what makes it additive: Longhorn's own
// "default-disk-<hash>" entry for the platform disk must survive a re-run untouched.
func TestLonghornDiskPatch(t *testing.T) {
	got, err := longhornDiskPatch([]domain.NodeDisk{
		{Name: "extra", MountPath: "/var/lib/longhorn-extra"},
		{Name: "bulk", MountPath: "/var/lib/longhorn-bulk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Disks map[string]struct {
				Path              string `json:"path"`
				DiskType          string `json:"diskType"`
				AllowScheduling   bool   `json:"allowScheduling"`
				EvictionRequested bool   `json:"evictionRequested"`
			} `json:"disks"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("patch is not valid JSON: %v (%s)", err, got)
	}
	if len(doc.Spec.Disks) != 2 {
		t.Fatalf("disks = %+v, want both", doc.Spec.Disks)
	}
	d, ok := doc.Spec.Disks["kaas-extra"]
	if !ok {
		t.Fatalf("disks = %+v, want the platform-namespaced key", doc.Spec.Disks)
	}
	if d.Path != "/var/lib/longhorn-extra" {
		t.Errorf("path = %q", d.Path)
	}
	if d.DiskType != "filesystem" {
		t.Errorf("diskType = %q, want filesystem - the node_disks role laid down a real filesystem", d.DiskType)
	}
	// Explicit, not defaulted: the same map drains a disk on the way out, so a re-run after a
	// cancelled removal has to put it back into service.
	if !d.AllowScheduling || d.EvictionRequested {
		t.Errorf("allowScheduling=%v evictionRequested=%v, want the disk in service", d.AllowScheduling, d.EvictionRequested)
	}
}
