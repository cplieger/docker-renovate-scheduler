package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/health"
	scheduler "github.com/cplieger/scheduler/v3"
)

// --- Daemon: the single owner of Renovate execution ---
//
// PID 1 owns every Renovate run as its own child process. Triggers only
// submit requests: the built-in ticker (built-in mode) and the unix-socket
// clients (`run` subcommand, both modes) all feed one FIFO queue served by
// one executor goroutine. That single-ownership is the design: mutual
// exclusion is the executor loop (nothing else may start Renovate), shutdown
// is ordinary parent/child draining plus explicit cancellation of the still-
// queued, and every run's output lands on the container log stream because
// the child inherits the daemon's stdout/stderr.

// daemon carries the executor's dependencies.
type daemon struct {
	queue  *runQueue
	marker *health.Marker
	newCmd scheduler.CommandRunner
	// runCtx parents every Renovate child. It is decoupled from the shutdown
	// signal (context.WithoutCancel) so an in-flight run drains to completion
	// — bounded by its own SCHED_TIMEOUT — instead of being cancelled
	// mid-pass; the container's stop_grace_period is the real outer bound.
	runCtx  context.Context
	timeout time.Duration
}

// runDaemon is the composition root for the long-running container (the
// `daemon` subcommand and the default no-arg command). It configures logging,
// verifies the Renovate base directory, binds the trigger socket, wires the
// health marker, starts the executor, and — in built-in mode — drives the
// interval ticker. newCmd builds each Renovate child (defaultCommandRunner in
// production; injected by tests). Returning an error exits non-zero.
func runDaemon(ctx context.Context, socketPath string, newCmd scheduler.CommandRunner) error {
	setupLogger()
	warnIfRootlessCacheUnwritable()

	if err := verifyBaseDir(ctx); err != nil {
		logBaseDirError(err)
		return err
	}

	interval, scheduleEnabled := loadInterval()
	timeout := loadRunTimeout()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := listenTrigger(socketPath)
	if err != nil {
		slog.Error("cannot bind trigger socket", "path", socketPath, "error", err)
		return err
	}
	defer func() { _ = os.Remove(socketPath) }()

	marker := health.NewMarker(healthMarkerPath)
	defer marker.Cleanup()
	// Built-in mode starts unhealthy until the first run proves the setup
	// (the startup pass flips it); external mode starts healthy — idle,
	// nothing has failed — and each triggered run updates it.
	marker.Set(!scheduleEnabled)

	d := &daemon{
		queue:   newRunQueue(queueCapacity),
		marker:  marker,
		newCmd:  newCmd,
		runCtx:  context.WithoutCancel(ctx),
		timeout: timeout,
	}

	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		d.runJobs(ctx)
	}()

	srv := &triggerServer{queue: d.queue}
	go srv.serve(ln)

	tickerDone := startTicker(ctx, d, interval, scheduleEnabled)

	mode := "external"
	if scheduleEnabled {
		mode = "built-in"
	}
	slog.Info("container started ("+mode+" scheduling)",
		"interval", interval, "timeout", timeout, "base_dir", baseDir(), "socket", socketPath)

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Mark unhealthy immediately so observers see the signal before the run
	// drain (a Renovate run can take a while).
	marker.Set(false)

	// Stop admission (socket + queue), then wait: the executor finishes the
	// in-flight run uncancelled (runCtx) and delivers cancellation results to
	// everything still queued; the ticker returns once its waiting tick job
	// is resolved; the handlers return once every accepted request has its
	// final event on the wire.
	_ = ln.Close()
	d.queue.close()
	<-executorDone
	<-tickerDone
	srv.handlers.Wait()
	slog.Info("shutdown complete")
	return nil
}

// startTicker runs the built-in interval scheduler: a startup run that fires
// immediately for freshness on deploy, then one run per interval, each
// submitted to the queue like any other trigger and waited on (RunLoop is
// sequential, so ticks can never pile up behind a long pass). Disabled
// (closed channel returned) in external mode. The library re-checks ctx
// before each fire, so no fresh tick is submitted after shutdown begins.
func startTicker(ctx context.Context, d *daemon, interval time.Duration, enabled bool) <-chan struct{} {
	done := make(chan struct{})
	if !enabled {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		startupDone := false
		scheduler.RunLoop(ctx, func(context.Context) {
			trigger := "interval"
			if !startupDone {
				trigger, startupDone = "startup", true
			}
			d.tick(trigger)
		}, scheduler.LoopOptions{Interval: interval, FireOnStart: true})
	}()
	return done
}

// tick submits one scheduled run and waits for its result (the executor sets
// the health marker; the queue guarantees exactly one result per accepted
// job, including a cancellation result at shutdown, so this wait always
// resolves). A rejected submission — the queue full of external requests, or
// shutdown racing the tick — is logged and skipped: the next interval
// provides freshness.
func (d *daemon) tick(trigger string) {
	j := newJob(trigger, nil, nil)
	if err := d.queue.submit(j); err != nil {
		slog.Warn("scheduled run skipped", "trigger", trigger, "reason", err)
		return
	}
	<-j.result
}

// runJobs is the executor: the only code that starts Renovate. It serves the
// queue strictly in order until the queue is closed and drained. Once
// shutdown is signalled, remaining jobs are cancelled — delivered as explicit
// not-ok results with a reason — instead of run, so a stop request is never
// followed by a fresh run.
func (d *daemon) runJobs(shutdownCtx context.Context) {
	for j := range d.queue.jobs {
		if shutdownCtx.Err() != nil {
			j.finish(runOutcome{ok: false, reason: "cancelled: scheduler shutting down"})
			continue
		}
		d.execute(j)
	}
}

// execute performs one job: signal the waiter, re-verify the base directory
// (a volume can degrade after boot; failing here beats a confusing Renovate
// error), run the pass, record the outcome on the health marker, and deliver
// the result.
func (d *daemon) execute(j *job) {
	close(j.started)
	start := time.Now()

	if err := verifyBaseDir(d.runCtx); err != nil {
		logBaseDirError(err)
		d.marker.Set(false)
		j.finish(runOutcome{ok: false, duration: time.Since(start), reason: "base directory not writable"})
		return
	}

	ok := runRenovateOnce(d.runCtx, d.timeout, j.trigger, j.repos, j.env, d.newCmd)
	d.marker.Set(ok)
	j.finish(runOutcome{ok: ok, duration: time.Since(start)})
}
