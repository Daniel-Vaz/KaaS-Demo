package kubectl

import (
	"context"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/audit"
)

// CacheTTL is how long a fetched audit window is reused before the next kubectl tail. Deliberately
// just under the portal's own poll cadence (5s), so a POLL still pays for a fresh read - what the
// cache absorbs is everything else hitting the same cluster in between: a verb/limit change, a
// debounced search, a second viewer, a second API replica's worth of tabs.
const CacheTTL = 4 * time.Second

// cacheEvictAfter prunes a cluster's entry once nobody has read it for a while - otherwise a deleted
// cluster's window would sit in the map for the life of the process.
const cacheEvictAfter = 5 * time.Minute

// cache holds the most recent audit window per cluster. The window is unfiltered: audit.Assemble
// applies the query ABOVE it, so one cached fetch serves every filter combination.
//
// Keyed on cluster ID alone, which is safe because an audit window is cluster-wide observed state
// read with the cluster's ADMIN kubeconfig for every caller - there is no per-user view of it to
// leak. Tenancy is enforced before the querier is ever reached (app.auditRead's view check).
//
// In-process and per-replica, which is fine under horizontal scaling: nothing is pinned to a replica,
// a miss just re-reads, and two replicas holding slightly different windows is no different from two
// polls landing a second apart.
type cache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	// sem is a one-slot lock held ACROSS the fetch, so concurrent readers of the same cluster don't
	// each launch their own tail - the losers wait and then hit the fresh window (singleflight
	// without the dependency). A channel rather than a mutex so the wait honours ctx cancellation.
	sem chan struct{}

	events    []audit.Event
	fetchedAt time.Time
	lastUsed  time.Time
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, entries: make(map[string]*cacheEntry)}
}

// window returns the cluster's audit window, calling fetch only if the cached one has expired.
func (c *cache) window(ctx context.Context, clusterID string, fetch func(context.Context) ([]audit.Event, error)) ([]audit.Event, error) {
	e := c.entryFor(clusterID)

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.mu.Lock()
	cached, fetchedAt := e.events, e.fetchedAt
	e.lastUsed = time.Now()
	c.mu.Unlock()

	if cached != nil && time.Since(fetchedAt) < c.ttl {
		return cached, nil
	}

	events, err := fetch(ctx)
	if err != nil {
		// Serve a stale window rather than an error if we have one: a momentarily unreachable control
		// plane shouldn't blank the feed mid-poll.
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}

	c.mu.Lock()
	e.events, e.fetchedAt = events, time.Now()
	c.mu.Unlock()
	return events, nil
}

// entryFor returns the cluster's entry, creating it if needed, and sweeps entries nobody has read
// recently (a cheap piggy-backed GC - there is no separate ticker to leader-elect).
func (c *cache) entryFor(clusterID string) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, e := range c.entries {
		if id != clusterID && now.Sub(e.lastUsed) > cacheEvictAfter {
			delete(c.entries, id)
		}
	}
	e := c.entries[clusterID]
	if e == nil {
		e = &cacheEntry{sem: make(chan struct{}, 1)}
		c.entries[clusterID] = e
	}
	e.lastUsed = now
	return e
}
