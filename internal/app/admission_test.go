package app

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/quota"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// newAdmissionApp builds a minimal App for the admission tests: a memory store, the shipped
// catalog, and a budget sized to exactly four "small" clusters (4 vCPU / 8192 MB each), so a
// concurrent burst has to be turned away partway through.
func newAdmissionApp(t *testing.T) *App {
	t.Helper()
	return &App{
		Store:   store.NewMemory(),
		Catalog: upgradeChainCatalog(t),
		ProviderBudgets: map[string]quota.Budget{
			domain.ProviderKVM: {TotalVCPU: 4 * 4, TotalMemMB: 4 * 8192, TotalDiskGB: 1 << 20},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestConcurrentCreateAdmission is the regression test for the horizontal-scaling race: admission
// reads every live cluster (to pick a free node network and to sum the owner's quota) and THEN
// writes one, so concurrent creates that interleave would each decide against a snapshot that the
// others invalidate - handing two clusters the same CIDR, and letting more clusters through than
// the budget allows. Both invariants are restored by serializing admission under
// store.LockAdmission (which is a real cross-process lock on Postgres, and a mutex here).
//
// The concurrency here stands in for what is, in production, several API replicas: same code path,
// same store, no coordination beyond the lock.
func TestConcurrentCreateAdmission(t *testing.T) {
	a := newAdmissionApp(t)
	// A tenant's budget is the quota granted to them (the platform total is only the admin's
	// ceiling), so grant exactly four small clusters' worth: 4 × (4 vCPU, 8192 MB).
	owner := &domain.User{ID: "u1", Username: "owner", Quotas: map[string]domain.ResourceQuota{
		domain.ProviderKVM: {VCPU: 4 * 4, MemMB: 4 * 8192, DiskGB: 1 << 16},
	}}
	if err := a.Store.CreateUser(owner); err != nil {
		t.Fatal(err)
	}

	const attempts = 8 // twice what the budget can hold
	var wg sync.WaitGroup
	var mu sync.Mutex
	var created []*domain.Cluster
	var rejected int

	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := a.CreateCluster(owner, CreateRequest{
				Name:      string(rune('a' + i)),
				Size:      "small",
				NodePools: pools(0),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rejected++
				return
			}
			created = append(created, c)
		}()
	}
	wg.Wait()

	// The budget holds exactly 4 single-node small clusters; the other 4 must be turned away.
	if len(created) != 4 || rejected != 4 {
		t.Errorf("budget for 4 clusters admitted %d and rejected %d - quota was not enforced across concurrent creates",
			len(created), rejected)
	}

	// Every admitted cluster must hold a node network no other cluster holds.
	seen := make(map[string]string, len(created))
	for _, c := range created {
		if c.NetworkCIDR == "" {
			t.Errorf("cluster %q got no node network", c.Name)
			continue
		}
		if other, dup := seen[c.NetworkCIDR]; dup {
			t.Errorf("clusters %q and %q were both allocated %s - the node network was double-allocated",
				other, c.Name, c.NetworkCIDR)
		}
		seen[c.NetworkCIDR] = c.Name
	}

	// And the platform's own view has to agree: quota is computed from the stored clusters.
	all, err := a.Store.ListClustersByOwner(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	usedCPU, usedMem, usedDisk := quota.Usage(all)
	ceiling := a.ProviderBudgets[domain.ProviderKVM]
	if usedMem > ceiling.TotalMemMB || usedCPU > ceiling.TotalVCPU || usedDisk > ceiling.TotalDiskGB {
		t.Errorf("stored clusters exceed the kvm ceiling: %d vCPU / %d MB / %d GB used, ceiling %d / %d / %d",
			usedCPU, usedMem, usedDisk, ceiling.TotalVCPU, ceiling.TotalMemMB, ceiling.TotalDiskGB)
	}
}
