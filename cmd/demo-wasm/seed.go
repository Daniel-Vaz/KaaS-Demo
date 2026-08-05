//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/app"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// The demo fleet.
//
// Seeding goes through the ordinary app API - CreateCluster, UpdateUser, CreateGroup, AddNodeDisk -
// and then waits for the reconciler to converge, rather than writing rows into the store directly.
// That is the point: what a visitor lands on is a fleet the platform actually built, with real
// phases, real events on every cluster's timeline, real quota charged, and real observed state. A
// hand-written fixture would be the one part of the demo that isn't the product.
//
// The whole thing runs before the portal mounts, which is why the tick interval is turned up
// (see demoEnv): the fleet below is roughly nine phase transitions deep.

// Demo credentials. Shown on the portal's login screen in demo builds - there is nothing to protect
// (every visitor gets a private instance) and a demo you cannot get into is not one.
const (
	DemoAdminUser     = "admin"
	DemoAdminPassword = "kubeharbor"
	// The two tenant accounts exist to make the read/write group split visible: both see the same
	// clusters through the same group, and only one of them can change anything.
	DemoWriterUser   = "alice"
	DemoReaderUser   = "bob"
	DemoUserPassword = "kubeharbor"
)

// seedTimeout bounds the wait for the fleet to converge. Exceeding it is not fatal - a half-built
// fleet is a working demo with clusters still coming up on screen, which is hardly a bad first
// impression - so the caller only logs it.
const seedTimeout = 30 * time.Second

func seed(ctx context.Context, a *app.App, log *slog.Logger) error {
	admin, err := a.AdminUser()
	if err != nil {
		return fmt.Errorf("demo admin: %w", err)
	}

	writer, err := seedTenants(a, admin)
	if err != nil {
		return err
	}

	// A custom catalog, owned by the writer and shared through the group: the tenant-facing
	// counterpart to the built-in catalog (internal/app/customcatalog.go).
	if err := seedCustomCatalog(a, writer); err != nil {
		log.Warn("demo custom catalog", "err", err)
	}

	// The steady-state fleet. One per infrastructure so the provider column has something to say,
	// and an HA cluster so the control-plane story is visible.
	fleet := []app.CreateRequest{
		{
			Name:      "harbor-prod",
			Size:      "medium",
			HA:        true,
			Provider:  domain.ProviderKVM,
			NodePools: []domain.NodePool{{Name: "default", Size: "medium", DesiredWorkers: 3}},
		},
		{
			Name:     "harbor-staging",
			Size:     "medium",
			Provider: domain.ProviderKVM,
			NodePools: []domain.NodePool{
				{Name: "default", Size: "small", DesiredWorkers: 2},
				// A second pool at a different size - the shape node pools exist for.
				{Name: "memory", Size: "large", DesiredWorkers: 1},
			},
		},
		{
			Name:      "analytics",
			Size:      "medium",
			Provider:  domain.ProviderVSphere,
			NodePools: []domain.NodePool{{Name: "default", Size: "medium", DesiredWorkers: 2}},
		},
		{
			Name:      "edge-eu-west",
			Size:      "small",
			Provider:  domain.ProviderProxmox,
			NodePools: []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 1}},
		},
	}

	created := make([]*domain.Cluster, 0, len(fleet))
	for _, req := range fleet {
		c, err := a.CreateCluster(writer, req)
		if err != nil {
			log.Warn("demo cluster", "name", req.Name, "err", err)
			continue
		}
		created = append(created, c)
	}
	if err := waitReady(ctx, a, writer, ids(created), seedTimeout); err != nil {
		return err
	}

	// An extra data disk on a running node: the level-triggered, non-destructive storage path
	// (internal/domain.NodeDisk), and the reason the Nodes tab has a detail pane. The cluster is
	// re-read rather than reused from above, because the value CreateCluster returned is desired
	// state at admission - its nodes are observed state and did not exist yet.
	if c := byName(created, "harbor-prod"); c != nil {
		if c, err := a.GetCluster(writer, c.ID); err == nil {
			if n := firstWorker(c); n != "" {
				if _, err := a.AddNodeDisk(writer, c.ID, app.AddNodeDiskRequest{
					VMName: n, Name: "data", SizeGB: 100, MountPath: "/var/lib/longhorn-data", FSType: domain.FSExt4,
				}); err != nil {
					log.Warn("demo node disk", "err", err)
				}
			}
		}
	}

	// Prime observed state. Metrics and health are swept on their own slow tickers (15s and 20s),
	// which is right for a platform and wrong for a first impression: without this the visitor's
	// opening screen has empty health panels and no resource usage for the first twenty seconds of
	// a demo. These are the same sweeps the ticker runs, just run once up front.
	a.Rec.CollectMetrics(ctx)
	a.Rec.CheckHealth(ctx)

	// Last, and deliberately not waited on: a cluster still being built, so the visitor's first
	// screen has a live event stream and a phase advancing on it rather than four static rows.
	if _, err := a.CreateCluster(writer, app.CreateRequest{
		Name:      "dev-sandbox",
		Size:      "small",
		Provider:  domain.ProviderKVM,
		NodePools: []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 1}},
	}); err != nil {
		log.Warn("demo cluster", "name", "dev-sandbox", "err", err)
	}
	return nil
}

// seedTenants creates the two tenant accounts, the group they share, and their capacity grants. It
// returns the writer, who owns the fleet; the reader needs nothing further, their whole point being
// what they can see of someone else's clusters and cannot do to them.
//
// Quota is granted per infrastructure and the conserved-pool invariant is enforced per backend, so
// these are deliberate slices of the ceilings in demoEnv rather than the whole thing: a visitor who
// opens the Capacity page should see a platform with headroom and an admin who holds no fixed slice.
func seedTenants(a *app.App, admin *domain.User) (writer *domain.User, err error) {
	if writer, err = a.Register(DemoWriterUser, DemoUserPassword); err != nil {
		return nil, fmt.Errorf("demo writer: %w", err)
	}
	reader, err := a.Register(DemoReaderUser, DemoUserPassword)
	if err != nil {
		return nil, fmt.Errorf("demo reader: %w", err)
	}

	group, err := a.CreateGroup(admin, "platform-engineering")
	if err != nil {
		return nil, fmt.Errorf("demo group: %w", err)
	}

	quotas := map[string]domain.ResourceQuota{
		domain.ProviderKVM:     {VCPU: 64, MemMB: 262144, DiskGB: 2048},
		domain.ProviderVSphere: {VCPU: 32, MemMB: 131072, DiskGB: 1024},
		domain.ProviderProxmox: {VCPU: 32, MemMB: 131072, DiskGB: 1024},
	}
	if writer, err = a.UpdateUser(admin, writer.ID, app.UpdateUserRequest{
		Quotas:      &quotas,
		Memberships: &[]domain.GroupMembership{{GroupID: group.ID, Role: domain.GroupRoleWrite}},
	}); err != nil {
		return nil, fmt.Errorf("demo writer grant: %w", err)
	}
	// The reader gets the same group at the lesser role and NO quota of their own: they can see and
	// use the writer's clusters, and cannot create any. That is the least-privileged default the
	// tenancy model is built around, not an oversight.
	if _, err = a.UpdateUser(admin, reader.ID, app.UpdateUserRequest{
		Memberships: &[]domain.GroupMembership{{GroupID: group.ID, Role: domain.GroupRoleRead}},
	}); err != nil {
		return nil, fmt.Errorf("demo reader membership: %w", err)
	}
	return writer, nil
}

func seedCustomCatalog(a *app.App, owner *domain.User) error {
	cat, err := a.CreateCustomCatalog(owner, "platform-charts")
	if err != nil {
		return err
	}
	_, err = a.AddCustomAddon(owner, cat.ID, domain.CustomAddon{
		Name:        "redis",
		Description: "In-memory data store, from the team's own chart repository.",
		Repo:        "https://charts.bitnami.com/bitnami",
		Chart:       "redis",
		Version:     "20.6.1",
		Namespace:   "data",
	})
	return err
}

// waitReady blocks until every named cluster is Ready, or until the timeout. It polls rather than
// subscribing to the event broker because a phase is what it is waiting on, and the cluster row is
// where a phase lives.
func waitReady(ctx context.Context, a *app.App, actor *domain.User, clusterIDs []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pending := 0
		for _, id := range clusterIDs {
			c, err := a.GetCluster(actor, id)
			if err != nil {
				return err
			}
			if c.Phase != domain.PhaseReady {
				pending++
			}
		}
		if pending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d demo clusters were still converging after %s", pending, len(clusterIDs), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func ids(cs []*domain.Cluster) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func byName(cs []*domain.Cluster, name string) *domain.Cluster {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// firstWorker names a cluster's first worker node - where the seeded extra disk goes. Control
// planes are in no pool and take no extra disks.
func firstWorker(c *domain.Cluster) string {
	for _, n := range c.Nodes {
		if n.Role == domain.RoleWorker {
			return n.VMName
		}
	}
	return ""
}
