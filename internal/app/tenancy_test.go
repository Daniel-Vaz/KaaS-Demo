package app

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/auth"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/quota"
	"github.com/Daniel-Vaz/KaaS-demo/internal/secrets"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// newTenancyApp builds an App on the in-memory store with the shipped catalog and a small platform
// budget, then seeds the admin (ensureAdmin), mirroring how New wires things - enough to exercise
// registration, quota, ownership, and user management without a database or real providers.
func newTenancyApp(t *testing.T) *App {
	t.Helper()
	cat, err := catalog.Default()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	box, err := secrets.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secrets box: %v", err)
	}
	a := &App{
		Store:   store.NewMemory(),
		Catalog: cat,
		// Single-provider deployment: the KVM host's ceiling is the whole platform.
		ProviderBudgets: map[string]quota.Budget{
			// Disk is deliberately ample: these tests bind on vCPU/memory, and a disk ceiling that
			// bit first would make them pass for the wrong reason.
			domain.ProviderKVM: {TotalVCPU: 32, TotalMemMB: 49152, TotalDiskGB: 1 << 20},
		},
		Signer:  auth.NewSigner("test-secret"),
		Secrets: box,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := a.ensureAdmin(); err != nil {
		t.Fatalf("ensureAdmin: %v", err)
	}
	return a
}

func admin(t *testing.T, a *App) *domain.User {
	t.Helper()
	u, err := a.AdminUser()
	if err != nil {
		t.Fatalf("admin user: %v", err)
	}
	return u
}

func TestAdminBootstrap(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	if !ad.IsAdmin {
		t.Fatal("seeded account should be admin")
	}
	// The admin holds no stored quota on any infrastructure - its budget on each is that
	// infrastructure's live unallocated pool, which starts as the whole ceiling since nobody has
	// been granted anything yet.
	if len(ad.Quotas) != 0 {
		t.Fatalf("admin should hold no stored quota, got %v", ad.Quotas)
	}
	budget, err := a.budgetFor(ad, domain.ProviderKVM)
	if err != nil {
		t.Fatalf("budgetFor: %v", err)
	}
	ceiling := a.ProviderBudgets[domain.ProviderKVM]
	if budget.TotalVCPU != ceiling.TotalVCPU || budget.TotalMemMB != ceiling.TotalMemMB {
		t.Fatalf("admin kvm budget = %d/%d, want the full kvm ceiling %d/%d",
			budget.TotalVCPU, budget.TotalMemMB, ceiling.TotalVCPU, ceiling.TotalMemMB)
	}
}

func TestBackfillOwners(t *testing.T) {
	a := newTenancyApp(t)
	// A cluster created before tenancy has no owner; a second ensureAdmin backfills it to the admin.
	_ = a.Store.CreateCluster(&domain.Cluster{ID: "legacy", Name: "legacy", OwnerID: ""})
	if err := a.ensureAdmin(); err != nil {
		t.Fatalf("re-run ensureAdmin: %v", err)
	}
	c, _ := a.Store.GetCluster("legacy")
	if c.OwnerID != admin(t, a).ID {
		t.Fatalf("legacy cluster owner = %q, want admin", c.OwnerID)
	}
}

func TestRegisterStartsWithZeroQuota(t *testing.T) {
	a := newTenancyApp(t)
	alice, err := a.Register("alice", "password")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if alice.IsAdmin || len(alice.Quotas) != 0 {
		t.Fatalf("new user should be non-admin with no quota on any infrastructure, got %+v", alice)
	}
	// With zero quota, creating a cluster is rejected.
	if _, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"}); err == nil {
		t.Fatal("a zero-quota user must not be able to create a cluster")
	}
}

func TestRegisterValidation(t *testing.T) {
	a := newTenancyApp(t)
	if _, err := a.Register("ab", "password"); err == nil {
		t.Fatal("too-short username should be rejected")
	}
	if _, err := a.Register("alice", "short"); err == nil {
		t.Fatal("too-short password should be rejected")
	}
	if _, err := a.Register("admin", "password"); err == nil {
		t.Fatal("duplicate username (admin) should be rejected")
	}
}

func TestLogin(t *testing.T) {
	a := newTenancyApp(t)
	_, _ = a.Register("alice", "password")
	if _, err := a.Login(t.Context(), "alice", "password", ""); err != nil {
		t.Fatalf("valid login failed: %v", err)
	}
	if _, err := a.Login(t.Context(), "alice", "nope", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("bad password = %v, want ErrInvalidCredentials", err)
	}
	if _, err := a.Login(t.Context(), "ghost", "password", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user = %v, want ErrInvalidCredentials", err)
	}
}

// grantQuota grants a tenant quota directly from the platform's unallocated pool - the admin
// itself is never touched, since it holds no stored quota (see budgetFor). Returns the
// freshly-loaded tenant - callers must use it as the actor, since quota admission reads the
// actor's current quota (in production the actor always comes fresh from the session, never a
// stale object).
func grantQuota(t *testing.T, a *App, userID string, vcpu, memMB int) *domain.User {
	t.Helper()
	return grantQuotaOn(t, a, userID, domain.ProviderKVM, vcpu, memMB)
}

// grantQuotaOn grants on a named infrastructure - quota is per-backend, so a grant always names one.
//
// The disk grant is derived from the vCPU one (generously: 200 GB per vCPU, where a default node is
// 50 GB per 2 vCPU) rather than taken as a parameter. Every one of these callers is testing
// vCPU/memory admission, and a disk grant that bound first would make them pass for the wrong
// reason. Disk admission has its own tests - see quota.TestCheckRejectsOnDiskAlone and
// TestCreateChargesDiskQuota.
func grantQuotaOn(t *testing.T, a *App, userID, provider string, vcpu, memMB int) *domain.User {
	t.Helper()
	ad := admin(t, a)
	q := map[string]domain.ResourceQuota{provider: {VCPU: vcpu, MemMB: memMB, DiskGB: vcpu * 200}}
	if _, err := a.UpdateUser(ad, userID, UpdateUserRequest{Quotas: &q}); err != nil {
		t.Fatalf("grant quota on %s: %v", provider, err)
	}
	u, err := a.Store.GetUser(userID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	return u
}

// seedSecret encrypts and stores a per-cluster secret, standing in for what the reconciler saves
// once a cluster is Ready (e.g. the admin kubeconfig the per-user mint reads, without driving a full
// reconcile).
func seedSecret(t *testing.T, a *App, clusterID string, kind domain.SecretKind, plaintext string) {
	t.Helper()
	ct, err := a.Secrets.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	if err := a.Store.SaveSecret(clusterID, kind, ct); err != nil {
		t.Fatalf("save secret: %v", err)
	}
}

func TestUpdateUserQuotaConservedPool(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")

	// The whole point of the auto-pool: granting alice quota works directly, with no need to touch
	// the admin's own (nonexistent) quota first.
	alice = grantQuota(t, a, alice.ID, 8, 8192)
	if got := alice.QuotaOn(domain.ProviderKVM); got.VCPU != 8 {
		t.Fatalf("alice kvm quota = %d, want 8", got.VCPU)
	}
	// The admin's remaining budget on that infrastructure shrinks by exactly what was granted.
	budget, err := a.budgetFor(ad, domain.ProviderKVM)
	if err != nil {
		t.Fatalf("budgetFor: %v", err)
	}
	if want := a.ProviderBudgets[domain.ProviderKVM].TotalVCPU - 8; budget.TotalVCPU != want {
		t.Fatalf("admin remaining kvm budget = %d, want %d", budget.TotalVCPU, want)
	}
	// Over-allocating past that infrastructure's ceiling is still rejected.
	over := map[string]domain.ResourceQuota{domain.ProviderKVM: {VCPU: a.ProviderBudgets[domain.ProviderKVM].TotalVCPU + 1}}
	if _, err := a.UpdateUser(ad, alice.ID, UpdateUserRequest{Quotas: &over}); err == nil {
		t.Fatal("granting quota beyond the infrastructure's ceiling should be rejected")
	}
	// A grant on an infrastructure this deployment doesn't offer is rejected - it would be capacity
	// that doesn't exist.
	unknown := map[string]domain.ResourceQuota{domain.ProviderVSphere: {VCPU: 1}}
	if _, err := a.UpdateUser(ad, alice.ID, UpdateUserRequest{Quotas: &unknown}); err == nil {
		t.Fatal("granting quota on a provider that isn't enabled should be rejected")
	}
	// Non-admin cannot set quota.
	zero := map[string]domain.ResourceQuota{domain.ProviderKVM: {}}
	if _, err := a.UpdateUser(alice, ad.ID, UpdateUserRequest{Quotas: &zero}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin UpdateUser = %v, want ErrForbidden", err)
	}
	// Quota can't be set on an admin target - its budget is always the computed unallocated pool.
	if _, err := a.UpdateUser(ad, ad.ID, UpdateUserRequest{Quotas: &zero}); err == nil {
		t.Fatal("setting quota on an admin target should be rejected")
	}
}

// A grant is per infrastructure and is spendable only there: KVM capacity buys nothing on vSphere.
func TestQuotaIsNotFungibleAcrossProviders(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")
	// Wipe the seeded cross-provider grant: this owner has KVM capacity only.
	owner.Quotas = map[string]domain.ResourceQuota{domain.ProviderKVM: {VCPU: 64, MemMB: 128 * 1024, DiskGB: 8192}}
	if err := a.Store.UpdateUser(owner); err != nil {
		t.Fatal(err)
	}

	if _, err := a.CreateCluster(owner, CreateRequest{Name: "k", Size: "small", Provider: domain.ProviderKVM}); err != nil {
		t.Fatalf("kvm cluster within the kvm grant: %v", err)
	}
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "v", Size: "small", Provider: domain.ProviderVSphere}); err == nil {
		t.Fatal("a vsphere cluster with no vsphere grant = nil error, want a rejection - KVM headroom cannot fund a vCenter VM")
	}

	// Granted vSphere capacity, the same request is admitted.
	owner.Quotas[domain.ProviderVSphere] = domain.ResourceQuota{VCPU: 8, MemMB: 8192, DiskGB: 200}
	if err := a.Store.UpdateUser(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "v", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.230"}); err != nil {
		t.Fatalf("vsphere cluster within the vsphere grant: %v", err)
	}
}

func TestOwnershipIsolation(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	bob, _ := a.Register("bob", "password")
	alice = grantQuota(t, a, alice.ID, 4, 8192)

	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("alice create: %v", err)
	}

	// Bob cannot see, edit, or delete alice's cluster - it looks like it doesn't exist.
	if _, err := a.GetCluster(bob, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob GetCluster = %v, want ErrNotFound", err)
	}
	if err := a.DeleteCluster(bob, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob DeleteCluster = %v, want ErrNotFound", err)
	}
	if _, err := a.UpdateCluster(bob, c.ID, UpdateRequest{NodePools: ptr(pools(1))}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob UpdateCluster = %v, want ErrNotFound", err)
	}

	// Alice sees only her own; the admin sees everything.
	aliceList, _ := a.ListClusters(alice)
	if len(aliceList) != 1 {
		t.Fatalf("alice sees %d clusters, want 1", len(aliceList))
	}
	bobList, _ := a.ListClusters(bob)
	if len(bobList) != 0 {
		t.Fatalf("bob sees %d clusters, want 0", len(bobList))
	}
	adminList, _ := a.ListClusters(ad)
	if len(adminList) != 1 {
		t.Fatalf("admin sees %d clusters, want 1", len(adminList))
	}
	if _, err := a.GetCluster(ad, c.ID); err != nil {
		t.Fatalf("admin GetCluster should succeed: %v", err)
	}
}

func TestNameUniquePerOwner(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	bob, _ := a.Register("bob", "password")
	alice = grantQuota(t, a, alice.ID, 4, 8192)
	bob = grantQuota(t, a, bob.ID, 4, 8192) // bob gets his own slice from the admin as well

	if _, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"}); err != nil {
		t.Fatalf("alice create dev: %v", err)
	}
	// Bob may reuse the name "dev" - uniqueness is per owner.
	if _, err := a.CreateCluster(bob, CreateRequest{Name: "dev", Size: "small"}); err != nil {
		t.Fatalf("bob should be able to reuse the name in his own namespace: %v", err)
	}
	// Alice cannot create a second "dev".
	if _, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"}); err == nil {
		t.Fatal("duplicate cluster name for the same owner should be rejected")
	}
}

func TestDeleteUserCascade(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 4, 8192)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("alice create: %v", err)
	}

	if err := a.DeleteUser(admin(t, a), alice.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := a.Store.GetUser(alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice still present after delete: %v", err)
	}
	got, _ := a.Store.GetCluster(c.ID)
	if got.Phase != domain.PhaseDeleting {
		t.Fatalf("alice's cluster phase = %s, want Deleting (cascade)", got.Phase)
	}
}

func TestDeleteUserGuards(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")

	if err := a.DeleteUser(ad, ad.ID); err == nil {
		t.Fatal("admin must not delete their own account")
	}
	if err := a.DeleteUser(alice, ad.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin DeleteUser = %v, want ErrForbidden", err)
	}
}

// TestOperationsAttributedToActor: create/scale/upgrade operations record who triggered them, so
// the Activity tab can show it (see recordOp).
func TestOperationsAttributedToActor(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384) // enough to scale a small cluster to two nodes

	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := a.UpdateCluster(alice, c.ID, UpdateRequest{NodePools: ptr(pools(1))}); err != nil {
		t.Fatalf("scale: %v", err)
	}
	ops, err := a.Operations(alice, c.ID)
	if err != nil {
		t.Fatalf("Operations: %v", err)
	}
	if len(ops) != 2 { // create + scale
		t.Fatalf("got %d operations, want 2", len(ops))
	}
	for _, op := range ops {
		if op.ActorID != alice.ID || op.ActorUsername != "alice" {
			t.Fatalf("operation %s not attributed to alice: actor_id=%q actor_username=%q",
				op.Kind, op.ActorID, op.ActorUsername)
		}
	}
}

func TestListUsersAdminOnly(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	if _, err := a.ListUsers(alice); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin ListUsers = %v, want ErrForbidden", err)
	}
	adm := admin(t, a)
	// The admin's own live cluster draws straight from the unallocated pool - the report must surface
	// it as AdminUsed so the "free to grant" figure doesn't overstate real headroom.
	if _, err := a.CreateCluster(adm, CreateRequest{Name: "adminbox", Size: "small"}); err != nil {
		t.Fatalf("admin CreateCluster: %v", err)
	}
	rep, err := a.ListUsers(adm)
	if err != nil {
		t.Fatalf("admin ListUsers: %v", err)
	}
	if len(rep.Users) != 2 { // admin + alice
		t.Fatalf("ListUsers returned %d users, want 2", len(rep.Users))
	}
	// The report carries one allocation entry per enabled infrastructure - that's the unit a grant
	// is actually checked against. The headline total is their sum.
	if len(rep.Allocation) != 1 || rep.Allocation[0].Provider != domain.ProviderKVM {
		t.Fatalf("report allocation = %+v, want one kvm entry", rep.Allocation)
	}
	ceiling := a.ProviderBudgets[domain.ProviderKVM]
	if rep.Allocation[0].TotalVCPU != ceiling.TotalVCPU || rep.TotalVCPU != ceiling.TotalVCPU {
		t.Fatalf("report kvm ceiling = %d (total %d), want %d",
			rep.Allocation[0].TotalVCPU, rep.TotalVCPU, ceiling.TotalVCPU)
	}
	// Alice holds no grant, so nothing is allocated to tenants, but the admin's small single-node
	// cluster consumes one node's worth of capacity, reported as AdminUsed (not Allocated).
	small := domain.Sizes["small"]
	if got := rep.Allocation[0].AdminUsedVCPU; got != small.CPUs {
		t.Fatalf("kvm AdminUsedVCPU = %d, want %d (admin's own small cluster)", got, small.CPUs)
	}
	if got := rep.Allocation[0].AdminUsedMemMB; got != small.MemMB {
		t.Fatalf("kvm AdminUsedMemMB = %d, want %d (admin's own small cluster)", got, small.MemMB)
	}
	if rep.Allocation[0].AllocatedVCPU != 0 {
		t.Fatalf("kvm AllocatedVCPU = %d, want 0 (admin usage is not a grant)", rep.Allocation[0].AllocatedVCPU)
	}
}

func ptr[T any](v T) *T { return &v }

// pools is the default node pool with n small workers - the shape these tests' clusters have now
// that workers live in pools rather than in a flat count.
func pools(n int) []domain.NodePool {
	return []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: n}}
}
