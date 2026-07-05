package kubectl

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// Real API-server shapes: a bound claim (granted MORE than it requested, and reporting its granted
// access modes in status), and a pending one that has neither volume nor capacity yet.
const pvcList = `{"items":[
  {"metadata":{"name":"uploads","namespace":"demo","creationTimestamp":"2026-01-01T00:00:00Z"},
   "spec":{"volumeName":"pvc-abc","storageClassName":"standard","volumeMode":"Filesystem",
           "accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"8Gi"}}},
   "status":{"phase":"Bound","capacity":{"storage":"10Gi"},"accessModes":["ReadWriteOnce"]}},
  {"metadata":{"name":"archive","namespace":"demo"},
   "spec":{"storageClassName":"slow","volumeMode":"Filesystem",
           "accessModes":["ReadWriteMany"],"resources":{"requests":{"storage":"100Gi"}}},
   "status":{"phase":"Pending"}}
]}`

func TestPVCsParsing(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{"get persistentvolumeclaims": pvcList}})
	ps, err := c.PVCs(context.Background(), cl, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d claims, want 2", len(ps))
	}

	// A bound claim reports what it was GRANTED (status.capacity), not what it asked for - the two
	// differ here on purpose.
	up := ps[0]
	if up.Status != "Bound" || up.Volume != "pvc-abc" {
		t.Errorf("uploads = %q %q, want Bound pvc-abc", up.Status, up.Volume)
	}
	if up.Capacity != "10Gi" || up.Requested != "8Gi" {
		t.Errorf("uploads capacity/requested = %q/%q, want 10Gi/8Gi", up.Capacity, up.Requested)
	}
	if len(up.AccessModes) != 1 || up.AccessModes[0] != "RWO" {
		t.Errorf("uploads access modes = %v, want [RWO]", up.AccessModes)
	}
	if up.StorageClass != "standard" {
		t.Errorf("uploads class = %q, want standard", up.StorageClass)
	}

	// A pending claim has no status.capacity, so capacity falls back to the request and it has no
	// volume; its access modes come from the spec.
	ar := ps[1]
	if ar.Status != "Pending" || ar.Volume != "" {
		t.Errorf("archive = %q %q, want Pending and no volume", ar.Status, ar.Volume)
	}
	if ar.Capacity != "100Gi" || ar.Requested != "100Gi" {
		t.Errorf("archive capacity/requested = %q/%q, want 100Gi/100Gi", ar.Capacity, ar.Requested)
	}
	if len(ar.AccessModes) != 1 || ar.AccessModes[0] != "RWX" {
		t.Errorf("archive access modes = %v, want [RWX]", ar.AccessModes)
	}
}

const pvcObj = `{"metadata":{"name":"uploads","namespace":"demo","labels":{"app":"web"},
   "annotations":{"volume.kubernetes.io/storage-provisioner":"ebs.csi.aws.com"}},
  "spec":{"volumeName":"pvc-abc","storageClassName":"standard","volumeMode":"Filesystem",
          "accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"8Gi"}}},
  "status":{"phase":"Bound","capacity":{"storage":"8Gi"},
            "conditions":[{"type":"Resizing","status":"True","reason":"Expanding","lastTransitionTime":"2026-01-01T01:00:00Z"}]}}`

const pvObj = `{"metadata":{"name":"pvc-abc","creationTimestamp":"2026-01-01T00:00:00Z"},
  "spec":{"capacity":{"storage":"8Gi"},"persistentVolumeReclaimPolicy":"Delete","storageClassName":"standard",
          "csi":{"driver":"ebs.csi.aws.com"}},
  "status":{"phase":"Bound"}}`

// Two pods in the namespace, only one of which mounts the claim.
const podsWithVolumes = `{"items":[
  {"metadata":{"name":"web-1","namespace":"demo"},
   "spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"uploads"}},{"configMap":{"name":"cfg"}}]}},
  {"metadata":{"name":"other-1","namespace":"demo"},
   "spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"something-else"}}]}},
  {"metadata":{"name":"novol-1","namespace":"demo"},"spec":{}}
]}`

func TestPVCDetailWithVolumeAndPods(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{
		"get persistentvolumeclaims uploads": pvcObj,
		"get persistentvolumes pvc-abc":      pvObj,
		"get pods":                           podsWithVolumes,
	}})
	d, err := c.PVC(context.Background(), cl, nil, kube.PVCRef{Namespace: "demo", Name: "uploads"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Labels["app"] != "web" || d.Annotations["volume.kubernetes.io/storage-provisioner"] != "ebs.csi.aws.com" {
		t.Errorf("labels/annotations = %v / %v", d.Labels, d.Annotations)
	}
	if len(d.Conditions) != 1 || d.Conditions[0].Type != "Resizing" {
		t.Errorf("conditions = %+v, want one Resizing", d.Conditions)
	}
	if d.PersistentVolume == nil {
		t.Fatal("no persistent volume resolved")
	}
	if d.PersistentVolume.ReclaimPolicy != "Delete" || d.PersistentVolume.Source != "csi: ebs.csi.aws.com" {
		t.Errorf("pv = %+v, want Delete + csi source", d.PersistentVolume)
	}
	// Only the pod that actually mounts the claim counts.
	if len(d.UsedBy) != 1 || d.UsedBy[0] != "web-1" {
		t.Errorf("used_by = %v, want [web-1]", d.UsedBy)
	}
}

// A claim whose PV read fails must still render - the PV is best-effort enrichment.
func TestPVCDetailSurvivesVolumeLookupFailure(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{
		"get persistentvolumeclaims uploads": pvcObj,
		"get pods":                           podsWithVolumes,
		// no stub for `get persistentvolumes` - the stub execer answers a non-zero exit
	}})
	d, err := c.PVC(context.Background(), cl, nil, kube.PVCRef{Namespace: "demo", Name: "uploads"})
	if err != nil {
		t.Fatalf("a PV lookup failure should not fail the claim: %v", err)
	}
	if d.PersistentVolume != nil {
		t.Error("expected no PV when the lookup fails")
	}
	if d.Name != "uploads" {
		t.Errorf("claim = %q, want uploads", d.Name)
	}
}

const scList = `{"items":[
  {"metadata":{"name":"standard","creationTimestamp":"2026-01-01T00:00:00Z",
    "annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},
   "provisioner":"ebs.csi.aws.com","reclaimPolicy":"Delete","volumeBindingMode":"WaitForFirstConsumer",
   "allowVolumeExpansion":true,"parameters":{"type":"gp3"}},
  {"metadata":{"name":"beta-default",
    "annotations":{"storageclass.beta.kubernetes.io/is-default-class":"true"}},
   "provisioner":"kubernetes.io/aws-ebs","reclaimPolicy":"Retain","mountOptions":["noatime"]},
  {"metadata":{"name":"plain"},"provisioner":"csi.hostpath.k8s.io"}
]}`

func TestStorageClassesParsing(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{"get storageclasses": scList}})
	scs, err := c.StorageClasses(context.Background(), cl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scs) != 3 {
		t.Fatalf("got %d classes, want 3", len(scs))
	}

	std := scs[0]
	if !std.IsDefault || !std.AllowExpansion {
		t.Errorf("standard = default:%v expansion:%v, want both true", std.IsDefault, std.AllowExpansion)
	}
	if std.VolumeBindingMode != "WaitForFirstConsumer" || std.Parameters["type"] != "gp3" {
		t.Errorf("standard = %+v", std)
	}

	// The beta default-class annotation is still honoured.
	if !scs[1].IsDefault {
		t.Error("beta-default should be recognised as the default class")
	}
	if len(scs[1].MountOptions) != 1 || scs[1].MountOptions[0] != "noatime" {
		t.Errorf("beta-default mount options = %v", scs[1].MountOptions)
	}

	// No annotation and no allowVolumeExpansion → neither default nor expandable.
	if scs[2].IsDefault || scs[2].AllowExpansion {
		t.Errorf("plain = default:%v expansion:%v, want both false", scs[2].IsDefault, scs[2].AllowExpansion)
	}
}

const pvcEvents = `{"items":[
  {"type":"Warning","reason":"ProvisioningFailed","message":"no volume plugin matched","count":5,"lastTimestamp":"2026-01-01T02:00:00Z","involvedObject":{"kind":"PersistentVolumeClaim","name":"archive"}},
  {"type":"Normal","reason":"WaitForFirstConsumer","message":"waiting for consumer","count":1,"lastTimestamp":"2026-01-01T01:00:00Z","involvedObject":{"kind":"PersistentVolumeClaim","name":"archive"}}
]}`

func TestPVCEventsSorted(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{"get events": pvcEvents}})
	ev, err := c.PVCEvents(context.Background(), cl, nil, kube.PVCRef{Namespace: "demo", Name: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 {
		t.Fatalf("got %d events, want 2", len(ev))
	}
	// Newest first.
	if ev[0].Reason != "ProvisioningFailed" || ev[0].Count != 5 {
		t.Errorf("first event = %+v, want ProvisioningFailed count 5", ev[0])
	}
	if ev[0].Object != "PersistentVolumeClaim/archive" {
		t.Errorf("event object = %q", ev[0].Object)
	}
}

func TestShortAccessModes(t *testing.T) {
	got := shortAccessModes([]string{"ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany", "ReadWriteOncePod", "Weird"})
	want := []string{"RWO", "ROX", "RWX", "RWOP", "Weird"} // an unknown mode passes through verbatim
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mode %d = %q, want %q", i, got[i], want[i])
		}
	}
}
