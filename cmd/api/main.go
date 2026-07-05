// Command api runs the control-plane HTTP server together with the reconciler, so you
// can create clusters over REST and watch them converge over SSE.
//
//	go run ./cmd/api
//	curl -XPOST localhost:8080/clusters -d '{"name":"demo","size":"small","workers":2,"addons":["metrics-server"]}'
//	curl -N localhost:8080/clusters/<id>/events
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"

	"github.com/Daniel-Vaz/KaaS-demo/internal/api"
	"github.com/Daniel-Vaz/KaaS-demo/internal/app"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	a, err := app.New(log)
	if err != nil {
		log.Error("init app", "err", err)
		os.Exit(1)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Reconciler runs in-process alongside the API for the demo. In production (and in
	// `make up`) a separate host-networked worker owns reconciliation against the same
	// Postgres - set KAAS_DISABLE_RECONCILER=1 so the API doesn't also reconcile (it would
	// otherwise do so with fake providers). With Postgres this is River's durable job queue;
	// otherwise the in-memory tick loop.
	if os.Getenv("KAAS_DISABLE_RECONCILER") == "1" {
		log.Info("reconciler disabled in this process (KAAS_DISABLE_RECONCILER=1); a separate worker must run it")
	} else if err := a.StartReconciler(ctx); err != nil {
		log.Error("start reconciler", "err", err)
		os.Exit(1)
	}

	addr := ":8080"
	if v := os.Getenv("KAAS_ADDR"); v != "" {
		addr = v
	}
	srv := &http.Server{Addr: addr, Handler: api.NewServer(a, log).Routes()}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Info("api listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
