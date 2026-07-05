// Package events is the provisioning timeline: an append-only stream per cluster that
// the reconciler writes and the API tails over SSE (see docs/architecture.md).
//
// Broker fans events out to local SSE subscribers. In fake mode (or the worker's
// KAAS_SEED_DEMO dev path) the reconciler and the subscriber share one process, so a plain
// in-memory history + fan-out (NewBroker) is enough. In real mode the reconciler runs in a
// separate, host-networked worker process, so an in-memory broker in the worker would emit
// into a void nobody in the API process ever sees. NewPostgresBroker covers that case: it
// persists events to the `events` table and relays them via LISTEN/NOTIFY, so the API
// process's broker (constructed the same way) picks up events emitted by the worker's.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	ClusterID string    `json:"cluster_id"`
	TS        time.Time `json:"ts"`
	Level     string    `json:"level"`  // info | warn | error
	Source    string    `json:"source"` // infra | ansible | addon | reconciler
	Message   string    `json:"message"`
}

// Sink is where the reconciler emits events.
type Sink interface {
	Emit(e Event)
}

const pgChannel = "kaas_events"

// Broker is a Sink plus fan-out to SSE subscribers, with replay of history on subscribe.
// The zero-ish value (via NewBroker) keeps everything in memory; NewPostgresBroker backs it
// with Postgres instead so the broker works across processes.
type Broker struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}

	history map[string][]Event // in-memory mode only (pool == nil)

	pool *pgxpool.Pool // set in Postgres mode; persists + relays instead of using history
	log  *slog.Logger
}

func NewBroker() *Broker {
	return &Broker{
		subs:    map[string]map[chan Event]struct{}{},
		history: map[string][]Event{},
	}
}

// NewPostgresBroker persists events to Postgres and relays them via LISTEN/NOTIFY, so
// callers in another process (the real-mode worker emitting; the API subscribing) share one
// event stream. It starts a background goroutine that holds a dedicated connection and
// reconnects on failure; that goroutine runs for the life of ctx.
func NewPostgresBroker(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) *Broker {
	b := &Broker{
		subs: map[string]map[chan Event]struct{}{},
		pool: pool,
		log:  log,
	}
	go b.listenLoop(ctx)
	return b
}

func (b *Broker) Emit(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if b.pool != nil {
		b.persist(e)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.history[e.ClusterID] = append(b.history[e.ClusterID], e)
	b.broadcastLocked(e)
}

// persist inserts the event and notifies listeners in one round trip; the notify only fires
// once the insert commits. Live subscribers in this process (and any other) learn of it via
// listenLoop, which also does the local broadcast - so persist itself doesn't broadcast.
func (b *Broker) persist(e Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		b.log.Error("events: marshal", "err", err)
		return
	}
	const q = `
		WITH ins AS (
			INSERT INTO events (cluster_id, ts, level, source, message)
			VALUES ($1, $2, $3, $4, $5)
		)
		SELECT pg_notify($6, $7)`
	if _, err := b.pool.Exec(context.Background(), q,
		e.ClusterID, e.TS, e.Level, e.Source, e.Message, pgChannel, string(payload),
	); err != nil {
		b.log.Error("events: persist", "cluster", e.ClusterID, "err", err)
	}
}

// listenLoop holds a dedicated connection LISTENing on pgChannel and broadcasts whatever it
// hears to local subscribers. It reconnects (with a short backoff) if the connection drops.
func (b *Broker) listenLoop(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := b.pool.Acquire(ctx)
		if err != nil {
			b.log.Error("events: acquire listen conn", "err", err)
			sleep(ctx, time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+pgChannel); err != nil {
			b.log.Error("events: listen", "err", err)
			conn.Release()
			sleep(ctx, time.Second)
			continue
		}
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				break
			}
			var e Event
			if err := json.Unmarshal([]byte(n.Payload), &e); err != nil {
				b.log.Error("events: unmarshal notification", "err", err)
				continue
			}
			b.mu.Lock()
			b.broadcastLocked(e)
			b.mu.Unlock()
		}
		conn.Release()
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// broadcastLocked fans e out to current subscribers for e.ClusterID. Callers must hold b.mu.
func (b *Broker) broadcastLocked(e Event) {
	for ch := range b.subs[e.ClusterID] {
		select {
		case ch <- e:
		default: // slow subscriber; drop rather than block the emitter
		}
	}
}

// Subscribe returns the cluster's event history plus a channel of subsequent live events and a
// cancel func the caller must invoke when done. History is returned separately (rather than
// pre-loaded into the channel) because it can exceed the channel's buffer - pushing it in here
// would silently drop everything past the buffer on a reconnect against a large history. The
// caller must emit history first, then drain the channel, to preserve order. Capturing history
// and registering the subscriber under the same lock leaves no gap: broadcastLocked can't
// interleave, so no live event is missed between the two.
func (b *Broker) Subscribe(clusterID string) ([]Event, <-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	history := b.replayLocked(clusterID)
	ch := make(chan Event, 64)
	if b.subs[clusterID] == nil {
		b.subs[clusterID] = map[chan Event]struct{}{}
	}
	b.subs[clusterID][ch] = struct{}{}
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if set, ok := b.subs[clusterID]; ok {
			delete(set, ch)
		}
		close(ch)
	}
	return history, ch, cancel
}

// replayLocked returns the history to prime a fresh subscriber with. Callers must hold b.mu.
func (b *Broker) replayLocked(clusterID string) []Event {
	if b.pool == nil {
		return b.history[clusterID]
	}
	rows, err := b.pool.Query(context.Background(),
		`SELECT ts, level, source, message FROM events WHERE cluster_id = $1 ORDER BY id`, clusterID)
	if err != nil {
		b.log.Error("events: load history", "cluster", clusterID, "err", err)
		return nil
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e := Event{ClusterID: clusterID}
		if err := rows.Scan(&e.TS, &e.Level, &e.Source, &e.Message); err != nil {
			b.log.Error("events: scan history", "cluster", clusterID, "err", err)
			continue
		}
		out = append(out, e)
	}
	return out
}
