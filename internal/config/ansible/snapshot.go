package ansible

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// SnapshotEtcd runs etcd-snapshot.yml - an ONLINE `etcdctl snapshot save` plus archives of
// /etc/kubernetes and /var/lib/kubelet - and returns the bundle it fetched, together with the
// revision and hash the playbook's verification step read back.
//
// The bundle is read into memory rather than streamed because the caller seals it whole
// (secrets.Box) and stores it as one row. That is the reason for the size ceiling: a multi-gigabyte
// keyspace would be a multi-gigabyte parameter on a single INSERT, which does not fail gracefully.
func (m *Manager) SnapshotEtcd(ctx context.Context, c *domain.Cluster) (domain.EtcdSnapshot, []byte, error) {
	art, err := m.prep(c)
	if err != nil {
		return domain.EtcdSnapshot{}, nil, err
	}
	bundlePath := filepath.Join(art, "etcd-snapshot-bundle.tar.gz")
	// A bundle left by a previous run must never be mistaken for this one's output: the playbook
	// could fail before fetching, and storing the older archive under a fresh timestamp would mean
	// the platform believes it has a backup from a moment it does not.
	_ = os.Remove(bundlePath)

	if err := m.playbook(ctx, c, "etcd-snapshot.yml", map[string]any{"artifacts_dir": art}); err != nil {
		return domain.EtcdSnapshot{}, nil, err
	}

	snap, err := readSnapshotStatus(art)
	if err != nil {
		return domain.EtcdSnapshot{}, nil, err
	}
	payload, err := os.ReadFile(bundlePath)
	if err != nil {
		return domain.EtcdSnapshot{}, nil, fmt.Errorf("ansible: read etcd snapshot bundle: %w", err)
	}
	// The bundle holds the cluster's entire keyspace in plaintext. It has served its purpose the
	// moment it is in memory on its way to being sealed; leaving it on the worker's filesystem would
	// keep an unsealed copy of every Secret around for the next process that reads the artifacts dir.
	defer os.Remove(bundlePath)

	if int64(len(payload)) > domain.MaxEtcdSnapshotBytes {
		return domain.EtcdSnapshot{}, nil, fmt.Errorf(
			"ansible: etcd snapshot bundle is %s, over the %s limit - this cluster's keyspace needs real object storage, not the platform database",
			domain.HumanBytes(int64(len(payload))), domain.HumanBytes(domain.MaxEtcdSnapshotBytes))
	}
	snap.TakenAt = time.Now().UTC()
	snap.SizeBytes = int64(len(payload))
	snap.K8sVersion = c.K8sVersion
	// The play runs on control_plane[0], which is the first control-plane node in the generated
	// inventory - i.e. the first RoleControlPlane in c.Nodes. Record it, so the stored metadata says
	// which member the backup came from rather than leaving it blank (the fake fills it in, so an
	// empty node_name was a real/fake divergence).
	snap.NodeName = firstControlPlane(c)
	return snap, payload, nil
}

// firstControlPlane returns the VM name of the cluster's first control-plane node - the host the
// snapshot play targets (control_plane[0]).
func firstControlPlane(c *domain.Cluster) string {
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			return n.VMName
		}
	}
	return ""
}

// snapshotStatus mirrors `etcdutl snapshot status -w json`. Only the fields the platform reasons
// about are decoded; totalKey/totalSize are read for the same reason the endpoint status is - they
// are what make the stored metadata worth looking at.
type snapshotStatus struct {
	Hash      uint32 `json:"hash"`
	Revision  int64  `json:"revision"`
	TotalKey  int64  `json:"totalKey"`
	TotalSize int64  `json:"totalSize"`
}

// readSnapshotStatus parses the verification output the playbook left on the controller. A status
// that cannot be parsed, or that reports revision 0, is a HARD FAILURE rather than a snapshot stored
// without metadata: it means the file could not be read back, and a backup nobody has verified is
// exactly the kind that is discovered to be empty during a recovery.
func readSnapshotStatus(art string) (domain.EtcdSnapshot, error) {
	raw, err := os.ReadFile(filepath.Join(art, "etcd-snapshot-status.json"))
	if err != nil {
		return domain.EtcdSnapshot{}, fmt.Errorf("ansible: read etcd snapshot status: %w", err)
	}
	var st snapshotStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &st); err != nil {
		return domain.EtcdSnapshot{}, fmt.Errorf("ansible: parse etcd snapshot status: %w", err)
	}
	if st.Revision <= 0 {
		return domain.EtcdSnapshot{}, fmt.Errorf("ansible: etcd snapshot verification reported revision %d - refusing to store an unverifiable backup", st.Revision)
	}
	return domain.EtcdSnapshot{Revision: st.Revision, Hash: st.Hash}, nil
}

// RestoreEtcdSnapshot unpacks a stored bundle into the cluster's artifacts dir and runs
// restore-etcd-snapshot.yml against the freshly re-provisioned node.
//
// Unpacking here rather than in the playbook is what keeps the sealed payload opaque to everything
// above this seam: the store hands over bytes, this function is the only place that knows they are a
// tar of three files, and Ansible only ever sees ordinary files on disk.
func (m *Manager) RestoreEtcdSnapshot(ctx context.Context, c *domain.Cluster, node domain.Node, archive []byte) error {
	art, err := m.prep(c)
	if err != nil {
		return err
	}
	dir := filepath.Join(art, "restore")
	// Always from scratch: a leftover file from an earlier recovery attempt is a different cluster
	// state than the one being restored now, and half of each would be the worst possible outcome.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The unsealed bundle is on the worker's disk only for the duration of the play.
	defer os.RemoveAll(dir)

	if err := untarGz(archive, dir); err != nil {
		return fmt.Errorf("ansible: unpack etcd snapshot bundle: %w", err)
	}
	for _, want := range []string{"snapshot.db", "kube-etc.tar.gz", "kubelet.tar.gz"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			return fmt.Errorf("ansible: etcd snapshot bundle is missing %s: %w", want, err)
		}
	}
	extra := map[string]any{
		"target_node":   node.VMName,
		"target_ip":     node.IP,
		"artifacts_dir": art,
		"k8s_version":   c.K8sVersion,
		"k8s_minor":     minorStr(c.K8sVersion),
	}
	return m.playbook(ctx, c, "restore-etcd-snapshot.yml", extra)
}

// RestartKubelet runs repair-node.yml against one node: the cheapest rung of automatic node repair.
// A node that cannot be reached fails here, which is the intended outcome - the reconciler records
// the failed attempt and the ladder escalates to replacing it.
func (m *Manager) RestartKubelet(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	return m.playbook(ctx, c, "repair-node.yml", map[string]any{"target_node": node.VMName})
}

// untarGz extracts a gzipped tar into dir, flat: the bundle is three files at the top level and
// nothing else is expected. Entries naming a path outside dir are refused rather than sanitized -
// this archive is produced by the platform's own playbook, so a traversing path means the payload
// is not what it claims to be, and the right response to that is to stop.
func untarGz(archive []byte, dir string) error {
	zr, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // the bundle's "./" directory entry, and nothing else we want
		}
		name := filepath.Base(filepath.Clean(hdr.Name))
		if name == "." || name == ".." || name == string(filepath.Separator) {
			return fmt.Errorf("unexpected archive entry %q", hdr.Name)
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
}
