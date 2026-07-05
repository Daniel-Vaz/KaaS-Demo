package app

import (
	"fmt"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// Extra node disks (domain.NodeDisk): the per-node storage a user attaches to a running worker.
//
// These get their own endpoints rather than riding on UpdateCluster's whole-list-replace shape (as
// node pools do) for one reason: a disk is attached to ONE node, and the natural request is "add
// this disk to this node", not "here is the cluster's entire disk set". A whole-list PATCH would
// also make a lost update silently destroy someone else's disk - and destroying a disk destroys its
// data. Add and remove are therefore narrow, explicit operations.

// AddNodeDiskRequest is a new extra disk on one node.
type AddNodeDiskRequest struct {
	// VMName is the worker to attach it to (domain.Node.VMName).
	VMName string `json:"vm_name"`
	// Name is the disk's logical name, unique on that node. It names the LVM volume group.
	Name      string `json:"name"`
	SizeGB    int    `json:"size_gb"`
	MountPath string `json:"mount_path"`
	// FSType is domain.FSExt4 or domain.FSXFS; empty defaults to ext4.
	FSType string `json:"fs_type"`
}

// AddNodeDisk attaches a new extra disk to one of a cluster's worker nodes. Write-scoped.
//
// The cluster must be Ready. Two reasons, and the second is the load-bearing one:
//   - a disk is pinned to a specific node, and before a cluster is Ready its nodes are still being
//     built, so the user cannot meaningfully have chosen one;
//   - the reconciler converges disks from the Updating path, which is only reachable from Ready.
//     Accepting a disk mid-build would write desired state that nothing acts on until the cluster
//     next changes.
//
// The disk lands in "pending" and the generation is bumped; the reconciler creates the volume,
// attaches it, and formats/mounts it (see reconcile.mountNodeDisks).
func (a *App) AddNodeDisk(actor *domain.User, clusterID string, req AddNodeDiskRequest) (*domain.Cluster, error) {
	if _, err := a.authorizeClusterWrite(actor, clusterID); err != nil {
		return nil, err
	}
	if req.FSType == "" {
		req.FSType = domain.FSExt4
	}
	// The reserved name belongs to the platform's own per-worker storage disk, which is derived from
	// the cluster's StorageDiskGB - a user disk sharing it would be indistinguishable from that one
	// and would fight syncStorageDisks on every later admission.
	if err := domain.ValidateUserDiskName(req.Name); err != nil {
		return nil, err
	}
	// A disk with no mount path defaults to feeding the cluster's storage pool, which is what the
	// portal's "add disk" form offers and the overwhelmingly common reason to attach one. Mounting it
	// under the Longhorn data path is the whole registration mechanism (see NodeDisk.FeedsStoragePool);
	// naming any other path instead gives an ordinary filesystem Longhorn ignores.
	if req.MountPath == "" {
		req.MountPath = domain.LonghornMountPath(req.Name)
	}
	var c *domain.Cluster
	var op pendingOp
	// A disk is capacity, so this is an admission decision and must not interleave with another
	// replica's - same reasoning as CreateCluster/UpdateCluster. Re-read inside the lock: the copy
	// authorizeClusterWrite returned was read before it.
	if err := a.Store.WithLock(store.LockAdmission, func() error {
		fresh, err := a.Store.GetCluster(clusterID)
		if err != nil {
			return err
		}
		if fresh.Phase != domain.PhaseReady {
			return fmt.Errorf("cluster %q must be Ready to add a disk (phase %s)", fresh.Name, fresh.Phase)
		}
		disk := domain.NodeDisk{
			VMName:    req.VMName,
			Name:      req.Name,
			SizeGB:    req.SizeGB,
			MountPath: req.MountPath,
			FSType:    req.FSType,
			Phase:     domain.DiskPhasePending,
			// Minted here, at admission, so it is fixed for the disk's life and identical every time
			// it is recomputed. The provisioner pins it on the virtual disk and Ansible finds the
			// device by it (kvm); vsphere ignores it and reports its own identity back.
			WWN: domain.NewDiskWWN(fresh.ID, req.VMName, req.Name),
		}
		candidate := *fresh
		candidate.NodeDisks = append(append([]domain.NodeDisk(nil), fresh.NodeDisks...), disk)
		if err := domain.ValidateNodeDisks(&candidate, candidate.NodeDisks); err != nil {
			return err
		}
		if err := a.checkDiskCapacity(&candidate); err != nil {
			return err
		}
		fresh.NodeDisks = candidate.NodeDisks
		fresh.Generation++
		if err := a.Store.UpdateCluster(fresh); err != nil {
			return err
		}
		c = fresh
		op = pendingOp{domain.OpDisks,
			fmt.Sprintf("add disk %q (%d GB) to %s at %s", req.Name, req.SizeGB, req.VMName, req.MountPath), ""}
		return nil
	}); err != nil {
		return nil, err
	}
	a.recordOp(actor, c.ID, op.kind, c.Generation, op.summary, op.detail)
	return c, nil
}

// RemoveNodeDisk marks one of a node's extra disks for removal. Write-scoped.
//
// It flips the disk to "removing" rather than deleting the row, and that is deliberate: the row is
// what keeps the volume attached (it is part of the desired node spec) while the reconciler unmounts
// the filesystem and tears down its volume group IN THE GUEST. Only once that has run does the
// reconciler drop the row, which is what finally lets the volume be detached and destroyed. Deleting
// here instead would detach the device out from under a live mount.
//
// THE DATA IS DESTROYED. The API is the authoritative gate but does not second-guess the caller;
// the portal confirms first.
func (a *App) RemoveNodeDisk(actor *domain.User, clusterID, vmName, name string) (*domain.Cluster, error) {
	if _, err := a.authorizeClusterWrite(actor, clusterID); err != nil {
		return nil, err
	}
	var c *domain.Cluster
	var op pendingOp
	if err := a.Store.WithLock(store.LockAdmission, func() error {
		fresh, err := a.Store.GetCluster(clusterID)
		if err != nil {
			return err
		}
		if fresh.Phase == domain.PhaseDeleting || fresh.Phase.Terminal() {
			return fmt.Errorf("cluster %q is not editable (phase %s)", fresh.Name, fresh.Phase)
		}
		found := false
		for i, d := range fresh.NodeDisks {
			if d.VMName != vmName || d.Name != name {
				continue
			}
			// The platform's storage disk is derived state, not a thing a user owns: dropping the row
			// would only make the next admission mint it back, and in between the node would have lost
			// the storage its Longhorn replicas live on. Shrinking a cluster's storage means deleting
			// the node (or the cluster), which is an honest, explicit act.
			if d.IsPlatformStorage() {
				return fmt.Errorf("disk %q is the platform's per-worker storage disk and cannot be removed", name)
			}
			if d.Phase == domain.DiskPhaseRemoving {
				return nil // already on its way out - nothing to do, and no second operation logged
			}
			fresh.NodeDisks[i].Phase = domain.DiskPhaseRemoving
			found = true
			break
		}
		if !found {
			return store.ErrNotFound
		}
		fresh.Generation++
		if err := a.Store.UpdateCluster(fresh); err != nil {
			return err
		}
		c = fresh
		op = pendingOp{domain.OpDisks, fmt.Sprintf("remove disk %q from %s", name, vmName), ""}
		return nil
	}); err != nil {
		return nil, err
	}
	if op.kind == "" {
		return a.Store.GetCluster(clusterID) // the already-removing no-op above
	}
	a.recordOp(actor, c.ID, op.kind, c.Generation, op.summary, op.detail)
	return c, nil
}

// checkDiskCapacity prices the candidate against the owner's grant on this cluster's infrastructure
// and against that infrastructure's platform-wide ceiling - the same two gates a scale passes, since
// a disk spends the same conserved pool. Callers MUST hold store.LockAdmission: both checks read the
// live cluster set and then write.
func (a *App) checkDiskCapacity(candidate *domain.Cluster) error {
	ownerBudget, ownerClusters, err := a.ownerBudget(candidate.OwnerID, candidate.InfraProvider())
	if err != nil {
		return err
	}
	if err := ownerBudget.Check(clustersExcept(ownerClusters, candidate.ID), candidate); err != nil {
		return fmt.Errorf("%s: %w", candidate.InfraProvider(), err)
	}
	all, err := a.Store.ListClusters()
	if err != nil {
		return err
	}
	onProvider := clustersOnProvider(all, candidate.InfraProvider())
	return a.checkProviderCapacity(candidate.InfraProvider(), clustersExcept(onProvider, candidate.ID), candidate)
}
