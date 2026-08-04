package ansible

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// tarGz builds a gzipped tar from the given entries, in order, so untarGz is exercised against the
// same shape the etcd-snapshot play's bundle has - a "./" directory entry followed by regular files.
func tarGz(t *testing.T, entries map[string]string, dirEntries ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, d := range dirEntries {
		if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestUntarGzExtractsBundle covers the ordinary path: the three files the bundle carries, written
// flat into dir, with the archive's "./" directory entry skipped.
func TestUntarGzExtractsBundle(t *testing.T) {
	dir := t.TempDir()
	archive := tarGz(t, map[string]string{
		"./snapshot.db":     "db",
		"./kube-etc.tar.gz": "etc",
		"./kubelet.tar.gz":  "kubelet",
	}, "./")
	if err := untarGz(archive, dir); err != nil {
		t.Fatalf("untarGz: %v", err)
	}
	for name, want := range map[string]string{
		"snapshot.db": "db", "kube-etc.tar.gz": "etc", "kubelet.tar.gz": "kubelet",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestUntarGzRefusesEscapingEntry is the zipslip regression: an entry naming a path outside dir is
// REFUSED, not silently rewritten to its base name - a traversing path in a bundle the platform's
// own playbook wrote means the payload is not what it claims to be. The absolute-path and
// parent-relative forms are both checked, and nothing is written outside dir either way.
func TestUntarGzRefusesEscapingEntry(t *testing.T) {
	for _, name := range []string{"../escaped.db", "./nested/../../escaped.db", "/tmp/escaped.db", ".."} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "restore")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			err := untarGz(tarGz(t, map[string]string{name: "pwned"}), dir)
			if err == nil {
				t.Fatalf("untarGz(%q) = nil, want refusal", name)
			}
			if _, statErr := os.Stat(filepath.Join(parent, "escaped.db")); statErr == nil {
				t.Errorf("untarGz(%q) wrote outside dir", name)
			}
		})
	}
}
