// Advisory locks: the cross-process critical sections that let every component run with more
// than one replica (see docs/architecture.md#horizontal-scaling).
//
// Two shapes, both keyed by a name hashed to a 64-bit key:
//
//   - WithAdvisoryLock - a blocking mutual-exclusion section shared by every process on the same
//     database. It guards read-then-write admission (quota + IPAM in internal/app) and the
//     schema migrators, which are otherwise racy the moment a second api/worker replica starts.
//   - TryLease - a non-blocking leader lease. Exactly one holder platform-wide; used by the
//     worker's singleton loops (orphan GC, metrics, health), which must NOT run once per replica.
//
// Both take a dedicated connection out of the pool for the lock's lifetime, because a
// session-level advisory lock belongs to the session that took it - releasing the connection back
// to the pool while holding one would leak the lock onto whatever query ran next.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryKey hashes a lock name into the 64-bit key Postgres's advisory locks take. Every
// process derives the same key from the same name, which is the whole point.
func advisoryKey(name string) int64 {
	sum := sha256.Sum256([]byte("kaas-advisory:" + name))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// WithAdvisoryLock runs fn while holding the named lock, blocking until it is free. The lock is
// released even if fn panics, and it is released by Postgres anyway if this process dies - a
// crashed replica can't wedge the platform.
func WithAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, name string, fn func() error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire conn for lock %q: %w", name, err)
	}
	defer conn.Release()

	key := advisoryKey(name)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return fmt.Errorf("postgres: acquire lock %q: %w", name, err)
	}
	defer func() {
		// Best-effort on a fresh context: ctx may already be cancelled, and an unreleased
		// session lock would then only clear when the connection is torn down.
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key)
	}()
	return fn()
}

// Lease is a held leader lock. It stays valid until Release, or until the connection carrying it
// dies (a network blip, a Postgres restart) - at which point Postgres drops the lock and another
// replica may take it, so Done is closed and the holder must stop doing leader-only work.
type Lease struct {
	conn *pgxpool.Conn
	key  int64
	done chan struct{} // closed when the watcher has stopped touching conn
	stop chan struct{} // closed by Release to wind the watcher down

	stopOnce sync.Once
	relOnce  sync.Once
}

// leaseHeartbeat is how often a lease holder pings the connection carrying its lock. It bounds how
// long a replica can keep believing it is the leader after silently losing the lock.
const leaseHeartbeat = 5 * time.Second

// TryLease attempts to take the named leader lock without blocking. It returns (nil, nil) when
// another replica holds it - the caller should back off and try again later.
func TryLease(ctx context.Context, pool *pgxpool.Pool, name string) (*Lease, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: acquire conn for lease %q: %w", name, err)
	}
	key := advisoryKey(name)
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, fmt.Errorf("postgres: try lease %q: %w", name, err)
	}
	if !got {
		conn.Release()
		return nil, nil
	}
	l := &Lease{conn: conn, key: key, done: make(chan struct{}), stop: make(chan struct{})}
	go l.watch()
	return l, nil
}

// Done is closed when the lease is no longer held - either because the connection carrying it
// broke, or because Release was called.
func (l *Lease) Done() <-chan struct{} { return l.done }

// watch pings the lock-carrying connection until it fails or the lease is released. A failed ping
// means the session (and with it the advisory lock) is gone.
func (l *Lease) watch() {
	defer close(l.done)
	t := time.NewTicker(leaseHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), leaseHeartbeat)
			err := l.conn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// Release gives the lease up so another replica can take it. Safe to call twice, and safe after
// the lease was lost. It waits for the watcher to stop first: the watcher and the unlock share one
// connection, which must not be used concurrently.
func (l *Lease) Release() {
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
	l.relOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, l.key)
		l.conn.Release()
	})
}
