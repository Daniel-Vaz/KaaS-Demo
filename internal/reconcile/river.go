// River drives the reconciler through a durable, Postgres-backed job queue,
// replacing the inline tick loop when Postgres is configured. Design:
//
//   - The enqueuer is the level-trigger: every interval it inserts ONE unique job per
//     cluster that still needs work.
//   - A worker executes exactly one idempotent phase per job (reusing reconcileOne). On
//     error the job is retried with River's exponential backoff - no more hot-looping.
//   - Uniqueness is by ClusterID across the *active* states only (NOT Completed), so at
//     most one job per cluster is in flight, yet the next phase can be enqueued once the
//     current one finishes.
package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store/postgres"
)

// ReconcileArgs is a durable work item: "advance this cluster one step toward desired state."
type ReconcileArgs struct {
	ClusterID string `json:"cluster_id" river:"unique"`
}

func (ReconcileArgs) Kind() string { return "reconcile_cluster" }

type reconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	rec *Reconciler
}

func (w *reconcileWorker) Work(ctx context.Context, job *river.Job[ReconcileArgs]) error {
	c, err := w.rec.Store.GetCluster(job.Args.ClusterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // cluster gone; nothing to do
		}
		return err
	}
	// Advance one phase. Any error bubbles up so River retries with backoff.
	return w.rec.reconcileOne(ctx, c)
}

// River is the durable-queue orchestrator.
type River struct {
	client   *river.Client[pgx.Tx]
	pool     *pgxpool.Pool // also the leader lease's home (see leader.go)
	rec      *Reconciler
	interval time.Duration
	log      *slog.Logger
}

// NewRiver runs River's own migrations, registers the worker, and builds the client.
func NewRiver(ctx context.Context, pool *pgxpool.Pool, rec *Reconciler, log *slog.Logger, jobTimeout time.Duration) (*River, error) {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return nil, err
	}
	// Under the same advisory lock as our own schema migrator (internal/store/postgres): every
	// worker replica boots and migrates at once, and two concurrent migrators creating River's
	// tables would leave the loser dead on a duplicate-object error.
	if err := postgres.WithAdvisoryLock(ctx, pool, "river-migrate", func() error {
		_, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
		return err
	}); err != nil {
		return nil, err
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &reconcileWorker{rec: rec})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}},
		Workers: workers,
		// A single reconcile phase can legitimately run for minutes - WorkersReady installs the CNI
		// and add-ons with `helm --wait` (each up to 5m), and EnsureNodes uploads the cluster's base
		// image over the libvirt connection, which is a multi-GB transfer over an SSH tunnel when
		// KAAS_KVM_HOST is remote (KAAS_RECONCILE_JOB_TIMEOUT below exists because of exactly that -
		// see docs/networking.md#remote-kvm-hosts). River's default job timeout would otherwise
		// SIGKILL the reconcile mid-step - mid-Helm leaves the release stuck in "pending-install" (a
		// permanent "another operation in progress" loop); mid-upload just restarts the upload from
		// zero on retry, which never converges if the timeout is shorter than the transfer takes.
		JobTimeout: jobTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &River{client: client, pool: pool, rec: rec, interval: time.Second, log: log}, nil
}

// Start begins working jobs and launches the enqueue loop. Non-blocking.
//
// What is safe to run in EVERY worker replica and what is not:
//
//   - Job work and the enqueue loop are: uniqueness on ClusterID (across the active states) means
//     at most one job per cluster is in flight platform-wide no matter how many workers run, and a
//     duplicate insert from another replica's enqueue loop is deduped by the same constraint.
//   - The three ticker loops are NOT. They are per-process timers with nothing serializing them,
//     and GC in particular DESTROYS INFRASTRUCTURE - two replicas sweeping the same orphan would
//     run `tofu destroy` concurrently against one workspace. So they run only in the replica
//     holding the leader lease (see leaderLoop); the rest stand by, ready to take over.
func (o *River) Start(ctx context.Context) error {
	if err := o.client.Start(ctx); err != nil {
		return err
	}
	go o.enqueueLoop(ctx)
	go o.leaderLoop(ctx)
	o.log.Info("river reconciler started")
	return nil
}

// gcLoop runs the orphan sweep periodically. It's a plain in-process ticker (not a River
// job): GC is host-local infra reconciliation, not per-cluster durable work. Leader-only.
func (o *River) gcLoop(ctx context.Context) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.rec.GC(ctx)
		}
	}
}

// metricsLoop samples live resource-usage from Ready clusters periodically. Like gcLoop it's a
// plain in-process ticker, not a durable job: it's read-only telemetry served read-through, so a
// missed sample just means a slightly staler dashboard until the next tick.
func (o *River) metricsLoop(ctx context.Context) {
	t := time.NewTicker(metricsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.rec.CollectMetrics(ctx)
		}
	}
}

// healthLoop evaluates each Ready cluster's health checks periodically. Like metricsLoop it's a
// plain in-process ticker, not a durable job: it's read-only telemetry served read-through, and it
// never mutates desired state, so a missed evaluation just means a slightly staler health panel.
func (o *River) healthLoop(ctx context.Context) {
	t := time.NewTicker(healthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.rec.CheckHealth(ctx)
		}
	}
}

// Stop drains in-flight jobs.
func (o *River) Stop() { _ = o.client.Stop(context.Background()) }

func (o *River) enqueueLoop(ctx context.Context) {
	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.enqueueDue(ctx)
		}
	}
}

// uniqueActiveStates excludes Completed/Discarded/Cancelled so a finished job doesn't block
// enqueuing the cluster's next phase. (Available, Pending, Running, Scheduled are required.)
var uniqueActiveStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRunning,
	rivertype.JobStateRetryable,
	rivertype.JobStateScheduled,
}

func (o *River) enqueueDue(ctx context.Context) {
	clusters, err := o.rec.clustersNeedingWork()
	if err != nil {
		o.log.Error("river enqueue: list clusters", "err", err)
		return
	}
	for _, c := range clusters {
		res, err := o.client.Insert(ctx, ReconcileArgs{ClusterID: c.ID}, &river.InsertOpts{
			Queue:      river.QueueDefault,
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: uniqueActiveStates},
		})
		if err != nil {
			o.log.Error("river enqueue: insert", "cluster", c.ID, "err", err)
			continue
		}
		// A cluster the user asked to delete must not wait out a failing step's exponential
		// backoff. When a broken cluster hot-loops, its reconcile job is parked as `retryable`
		// minutes-to-hours in the future, and this fresh insert is deduped against it by the
		// ClusterID uniqueness - so without help the teardown wouldn't run until that backed-off
		// retry finally fires. Pull the parked job forward instead: the worker re-reads the cluster
		// and sees Phase=Deleting, so it destroys now. Delete supersedes whatever step was failing,
		// so bypassing its backoff is correct, not a hot-loop - and only Deleting is expedited, so a
		// still-converging cluster keeps its backoff and doesn't hammer the infra every tick.
		if c.Phase == domain.PhaseDeleting && res.UniqueSkippedAsDuplicate && jobParkedInFuture(res.Job) {
			if _, err := o.client.JobRetry(ctx, res.Job.ID); err != nil {
				o.log.Error("river enqueue: expedite delete", "cluster", c.ID, "job", res.Job.ID, "err", err)
			}
		}
	}
}

// jobParkedInFuture reports whether a job is waiting to run at a later time - a retryable job
// backed off after a failure, or one explicitly scheduled ahead - rather than already available
// or running. Only those need pulling forward: an available job will be worked on the next fetch,
// and JobRetry leaves a running job untouched.
func jobParkedInFuture(j *rivertype.JobRow) bool {
	if j == nil {
		return false
	}
	switch j.State {
	case rivertype.JobStateRetryable, rivertype.JobStateScheduled:
		return j.ScheduledAt.After(time.Now())
	default:
		return false
	}
}
