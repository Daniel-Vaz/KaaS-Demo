package app

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/quota"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// newSharedQuotaApp builds an App with per-user quota disabled (KAAS_SHARED_QUOTA) and a KVM ceiling
// sized to exactly two single-node "small" clusters (2 vCPU / 8192 MB each), so the shared pool has
// to turn away the third create whoever owns it.
func newSharedQuotaApp(t *testing.T) *App {
	t.Helper()
	return &App{
		Store:       store.NewMemory(),
		Catalog:     upgradeChainCatalog(t),
		SharedQuota: true,
		ProviderBudgets: map[string]quota.Budget{
			// memory binds at 2 clusters; disk is ample so it never binds first.
			domain.ProviderKVM: {TotalVCPU: 100, TotalMemMB: 2 * 8192, TotalDiskGB: 1 << 20},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestSharedQuotaZeroGrantUserCanCreate is the point of the feature: with per-user quota off, an
// account that was never granted any capacity still admits a cluster, because budgetFor hands it the
// whole provider ceiling instead of its (empty) personal grant. Under the normal model this same
// user - zero grant - would be rejected outright.
func TestSharedQuotaZeroGrantUserCanCreate(t *testing.T) {
	a := newSharedQuotaApp(t)
	u := &domain.User{ID: "u1", Username: "nobody"} // no Quotas at all
	if err := a.Store.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateCluster(u, CreateRequest{Name: "a", Size: "small"}); err != nil {
		t.Fatalf("shared-quota mode should let a zero-grant user create a cluster: %v", err)
	}

	// The same App with the flag off must reject it - proving the flag is what made the difference.
	a.SharedQuota = false
	if _, err := a.CreateCluster(u, CreateRequest{Name: "b", Size: "small"}); err == nil {
		t.Fatal("with per-user quota on, a zero-grant user must be rejected")
	}
}

// TestSharedQuotaPoolIsBoundedAcrossOwners proves the shared pool is still bounded by the backend
// ceiling AGGREGATE - across different owners, not per account. Two tenants fill the two-cluster
// pool between them; a third create by a third tenant is turned away even though that tenant has
// "the whole ceiling" as their nominal budget, because checkProviderCapacity counts every owner's
// live clusters on the backend. This is what keeps the host from being physically oversubscribed.
func TestSharedQuotaPoolIsBoundedAcrossOwners(t *testing.T) {
	a := newSharedQuotaApp(t)
	users := []*domain.User{
		{ID: "u1", Username: "alice"},
		{ID: "u2", Username: "bob"},
		{ID: "u3", Username: "carol"},
	}
	for _, u := range users {
		if err := a.Store.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.CreateCluster(users[0], CreateRequest{Name: "a", Size: "small"}); err != nil {
		t.Fatalf("first cluster should be admitted: %v", err)
	}
	if _, err := a.CreateCluster(users[1], CreateRequest{Name: "b", Size: "small"}); err != nil {
		t.Fatalf("second cluster should fill the pool exactly: %v", err)
	}
	// Pool is full (2 × 8192 MB == ceiling); a third, by a third owner, must be rejected.
	if _, err := a.CreateCluster(users[2], CreateRequest{Name: "c", Size: "small"}); err == nil {
		t.Fatal("shared pool overflowed: a create past the backend ceiling was admitted")
	}
}

// TestSharedQuotaRejectsGrants: while per-user quota is off, an admin can't set a grant - it would
// be dormant and misleading. UpdateUser rejects it with a clear message.
func TestSharedQuotaRejectsGrants(t *testing.T) {
	a := newSharedQuotaApp(t)
	admin := &domain.User{ID: "admin", Username: "admin", IsAdmin: true}
	target := &domain.User{ID: "u1", Username: "alice"}
	for _, u := range []*domain.User{admin, target} {
		if err := a.Store.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	grant := map[string]domain.ResourceQuota{domain.ProviderKVM: {VCPU: 4, MemMB: 8192}}
	if _, err := a.UpdateUser(admin, target.ID, UpdateUserRequest{Quotas: &grant}); err == nil {
		t.Fatal("setting a per-user grant while KAAS_SHARED_QUOTA is on must be rejected")
	}

	// A membership-only edit (no Quotas) must still work - the rejection is scoped to grants.
	empty := []domain.GroupMembership{}
	if _, err := a.UpdateUser(admin, target.ID, UpdateUserRequest{Memberships: &empty}); err != nil {
		t.Fatalf("a non-quota edit should still be allowed in shared mode: %v", err)
	}
}

// TestSharedQuotaReportFlag: the admin dashboard payload advertises the mode so the portal can swap
// the grant editor for a consumption view.
func TestSharedQuotaReportFlag(t *testing.T) {
	a := newSharedQuotaApp(t)
	admin := &domain.User{ID: "admin", Username: "admin", IsAdmin: true}
	if err := a.Store.CreateUser(admin); err != nil {
		t.Fatal(err)
	}
	rep, err := a.ListUsers(admin)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.SharedQuota {
		t.Fatal("UsersReport.SharedQuota should be true when KAAS_SHARED_QUOTA is on")
	}

	a.SharedQuota = false
	rep, err = a.ListUsers(admin)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SharedQuota {
		t.Fatal("UsersReport.SharedQuota should be false when per-user quota is on")
	}
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"", false, false},
		{"", true, true},
		{"1", false, true},
		{"true", false, true},
		{"TRUE", false, true},
		{"yes", false, true},
		{"on", false, true},
		{"0", true, false},
		{"false", true, false},
		{"off", true, false},
		{"garbage", true, true}, // unparseable falls back to def
	}
	for _, c := range cases {
		t.Setenv("KAAS_TEST_BOOL", c.val)
		if c.val == "" {
			// t.Setenv can't unset; emulate "absent" by checking the def path directly.
			if got := envBool("KAAS_DEFINITELY_UNSET_XYZ", c.def); got != c.want {
				t.Errorf("envBool(unset, %v) = %v, want %v", c.def, got, c.want)
			}
			continue
		}
		if got := envBool("KAAS_TEST_BOOL", c.def); got != c.want {
			t.Errorf("envBool(%q, %v) = %v, want %v", c.val, c.def, got, c.want)
		}
	}
}
