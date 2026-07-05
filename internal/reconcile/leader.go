// Leader election for the reconciler's singleton loops.
//
// The per-cluster reconcile work is already safe to spread over many worker replicas - River's
// unique-by-ClusterID constraint serializes it in the database. The three background tickers are
// not: they are plain timers, one set per process, and running them once per replica is at best
// wasteful (N× the kubectl load from the metrics/health sweeps) and at worst destructive (two
// replicas' orphan GC running `tofu destroy` on the same workspace at the same time).
//
// So exactly one replica runs them: the one holding a Postgres advisory lock (postgres.TryLease).
// The lock lives with the connection that took it, so a leader that crashes or is disconnected
// drops it immediately - Postgres does the failure detection, and another replica takes over on
// its next attempt. There is no fencing token here: the loops are idempotent and re-run every tick
// anyway, which is the same property the whole control loop rests on.
package reconcile

import (
	"context"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/store/postgres"
)

const (
	// leaderLock is the advisory-lock name the singleton loops elect on.
	leaderLock = "reconcile-singletons"
	// leaderRetry is how long a stand-by replica waits before trying to take the lease again.
	leaderRetry = 10 * time.Second
)

// leaderLoop campaigns for leadership forever: while it holds the lease it runs the singleton
// loops; when it loses it (or never gets it) it stands by and retries. Returns when ctx is done.
func (o *River) leaderLoop(ctx context.Context) {
	for ctx.Err() == nil {
		lease, err := postgres.TryLease(ctx, o.pool, leaderLock)
		if err != nil {
			o.log.Warn("leader election failed - retrying", "err", err, "in", leaderRetry)
		}
		if err != nil || lease == nil { // another replica leads; stand by
			select {
			case <-ctx.Done():
				return
			case <-time.After(leaderRetry):
			}
			continue
		}

		// Leader. Run the singleton loops under a context that also dies if the lease does, so we
		// stop sweeping the moment another replica may have started.
		o.log.Info("elected leader - running orphan GC, metrics, health and vault-sync loops")
		lctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-lease.Done():
				cancel()
			case <-lctx.Done():
			}
		}()
		o.runSingletons(lctx)
		cancel()
		lease.Release()
		if ctx.Err() == nil {
			o.log.Warn("lost leader lease - standing by")
		}
	}
}

// runSingletons runs the leader-only loops until ctx is cancelled (shutdown, or a lost lease).
func (o *River) runSingletons(ctx context.Context) {
	done := make(chan struct{}, 4)
	run := func(fn func(context.Context)) {
		go func() {
			fn(ctx)
			done <- struct{}{}
		}()
	}
	run(o.gcLoop)
	run(o.metricsLoop)
	run(o.healthLoop)
	run(o.vaultSyncLoop)
	for range 4 {
		<-done
	}
}

// vaultSyncLoop ensures the platform Vault objects once, then converges the per-user/-group access
// bindings on a ticker. Leader-only: the identity/policy writes are singleton work (they read the
// whole users/groups/clusters set and rewrite Vault), so running them once per replica would be
// wasteful churn - the same reasoning as the GC/metrics/health sweeps.
func (o *River) vaultSyncLoop(ctx context.Context) {
	o.rec.EnsureVaultPlatform(ctx)
	o.rec.SyncVaultAccess(ctx)
	t := time.NewTicker(vaultSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.rec.SyncVaultAccess(ctx)
		}
	}
}
