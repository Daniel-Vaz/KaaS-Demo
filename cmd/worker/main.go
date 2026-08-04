// Command worker runs the reconciler headless - the component that (in the real topology)
// owns reconciliation and is the ONLY one that touches libvirt/KVM. It runs host-networked
// with the libvirt socket mounted (see docs/networking.md); the API enqueues work by bumping
// desired state in the shared Postgres and this worker drives it via River.
//
//	make up                                # runs this as a container with real providers
//	KAAS_SEED_DEMO=1 go run ./cmd/worker   # dev: seed one demo cluster and watch it converge
//
// Modes:
//   - default: start the reconciler and run until signalled (the service/container role).
//   - KAAS_SEED_DEMO=1: also seed a demo cluster and print its event timeline, exiting once
//     it reaches Ready - the fastest way to watch the pattern converge without the API.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/app"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/version"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	// Same first line as the API: the worker's version must be readable from its logs alone,
	// because it is the replica that will be blamed for a bad reconcile.
	log.Info("kaas worker", "version", version.String())

	a, err := app.New(log)
	if err != nil {
		log.Error("init app", "err", err)
		os.Exit(1)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := a.StartReconciler(ctx); err != nil {
		log.Error("start reconciler", "err", err)
		os.Exit(1)
	}
	log.Info("worker reconciler started")

	// The worker is the only component with a network route to the cluster API servers, so it also
	// hosts the interactive-shell exec agent (a real bash+kubectl PTY the API proxies the browser
	// to). No-op unless KAAS_SHELL_LISTEN is set. See docs/networking.md.
	a.StartShellAgent(ctx)

	if os.Getenv("KAAS_SEED_DEMO") == "1" {
		seedAndWatch(ctx, a, log)
		return
	}

	// Service role: reconcile until signalled.
	<-ctx.Done()
	log.Info("worker shutting down")
}

// seedAndWatch creates one demo cluster, prints its events, and returns once it is Ready.
func seedAndWatch(ctx context.Context, a *app.App, log *slog.Logger) {
	admin, err := a.AdminUser() // the demo seed acts as the platform admin
	if err != nil {
		log.Error("load admin", "err", err)
		os.Exit(1)
	}
	c, err := a.CreateCluster(admin, app.CreateRequest{
		Name: "demo", Size: "small",
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 2}},
		// Bundle omitted → latest supported; add-ons omitted → the bundle's add-ons.
	})
	if err != nil {
		log.Error("seed cluster", "err", err)
		os.Exit(1)
	}
	log.Info("seeded demo cluster", "id", c.ID, "name", c.Name)

	history, ch, cancel := a.Broker.Subscribe(c.ID)
	defer cancel()
	go func() {
		for _, e := range history {
			log.Info("event", "source", e.Source, "level", e.Level, "msg", e.Message)
		}
		for e := range ch {
			log.Info("event", "source", e.Source, "level", e.Level, "msg", e.Message)
		}
	}()

	waitUntilReady(ctx, a, c.ID, log)
}

func waitUntilReady(ctx context.Context, a *app.App, id string, log *slog.Logger) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur, err := a.Store.GetCluster(id)
			if err != nil {
				continue
			}
			if cur.Phase == "Ready" {
				time.Sleep(200 * time.Millisecond) // let the final events flush
				log.Info("cluster converged", "phase", cur.Phase, "nodes", len(cur.Nodes))
				return
			}
		}
	}
}
