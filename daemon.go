package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/health"
	scheduler "github.com/cplieger/scheduler/v3"
	"github.com/cplieger/scheduler/v3/trigger"
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

// errContainmentLost is runDaemon's fatal exit cause when a run's process
// group survives the post-run kill sweep: a package-manager tree may still
// be writing the base directory, so the daemon halts instead of admitting
// another run, and the non-zero exit lets the container restart policy reap
// the surviving tree.
var errContainmentLost = errors.New("renovate run process group survived the kill sweep")

// containmentLostReason is the not-ok outcome reason delivered to the run
// whose group survived the sweep and to every waiter still queued behind it.
const containmentLostReason = "failed: run process group survived the kill sweep; scheduler halting"

// queueCapacity bounds pending requests in the trigger broker's FIFO. The
// realistic trigger set is one periodic job plus a release-webhook burst, so
// 16 is generous headroom; a client hitting a full queue is rejected
// immediately with a clear reason (honest backpressure).
const queueCapacity = 16

// newJob builds one queued run request: the trigger label for logs, the
// positional repository slugs (empty means Renovate's own configuration
// decides), and the complete child environment (nil means inherit the
// daemon's own — ticker-submitted runs).
func newJob(trig string, repos, env []string) *trigger.Job[runPayload] {
	return trigger.NewJob(trig, runPayload{Repos: repos, Env: env})
}

// runOnceFunc executes one Renovate pass and reports whether it exited
// cleanly, whether shutdown cancelled it at start, and whether its process
// group survived the post-run kill sweep. It is the shared signature of
// daemon.runOnce and runDaemon's runOnce parameter; runRenovateOnce is the
// production value.
type runOnceFunc func(ctx, shutdownCtx context.Context, timeout time.Duration, trig string, repos, env []string, newCmd scheduler.CommandRunner) (ok, cancelled, groupSurvived bool)

// daemon carries the executor's dependencies.
type daemon struct {
	queue  *trigger.Queue[runPayload]
	marker *health.Marker
	newCmd scheduler.CommandRunner
	// runOnce executes one Renovate pass; nil means runRenovateOnce (the
	// production path). It exists as a seam for the containment-halt
	// regression tests only: a SIGKILL-surviving process group cannot be
	// fabricated from real test children, so the surviving-group report is
	// injected at this boundary.
	runOnce runOnceFunc
	// fatal delivers the executor's containment-loss error to runDaemon.
	// Buffered 1: the executor halts after its single send, so the send can
	// never block even if runDaemon is already past its receive.
	fatal chan error
	// runCtx parents every Renovate child. It is decoupled from the shutdown
	// signal (context.WithoutCancel) so an in-flight run drains to completion
	// — bounded by its own SCHED_TIMEOUT — instead of being cancelled
	// mid-pass; the container's stop_grace_period is the real outer bound.
	runCtx  context.Context
	timeout time.Duration
	// healthMu orders the shutdown health transition against per-run marker
	// updates: once beginShutdown flips stopping, a draining run's completion
	// must not write the marker back to healthy. The health library's own
	// mutex only serializes individual Set calls; it gives the shutdown write
	// no precedence, so the ordering guard lives here.
	healthMu sync.Mutex
	stopping bool
	// halted records containment loss executor-side. Only the executor
	// goroutine reads or writes it (runJobs and execute run on that one
	// goroutine), so it needs no lock: once set, every remaining and future
	// queued job is failed with containmentLostReason instead of run.
	halted bool
}

// beginShutdown records that shutdown has begun and marks the daemon
// unhealthy. After this, setRunHealth becomes a no-op: the shutdown state is
// final until Cleanup, no matter when a draining run completes.
func (d *daemon) beginShutdown() {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()
	d.stopping = true
	d.marker.Set(false)
}

// setRunHealth records a run outcome on the health marker, unless shutdown
// has already begun — a late completion of the draining in-flight run must
// not resurrect a healthy marker after observers were told the daemon is
// going down.
func (d *daemon) setRunHealth(ok bool) {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()
	if !d.stopping {
		d.marker.Set(ok)
	}
}

// runDaemon is the composition root for the long-running container (the
// `daemon` subcommand and the default no-arg command). It configures logging,
// verifies the Renovate base directory, binds the trigger socket, wires the
// health marker, starts the executor, and — in built-in mode — drives the
// interval ticker. newCmd builds each Renovate child (defaultCommandRunner in
// production; injected by tests). runOnce is copied into the daemon it
// builds; nil — the production value — selects runRenovateOnce, and a
// non-nil value exists only for the shutdown-ordering containment regression
// test (see daemon.runOnce). Returning an error exits non-zero.
func runDaemon(ctx context.Context, socketPath string, newCmd scheduler.CommandRunner, runOnce runOnceFunc) error {
	setupLogger()
	warnIfRootlessCacheUnwritable()

	// The marker is created up front so every boot-failure path can
	// overwrite a previous life's healthy marker (a docker restart
	// preserves /tmp; a crash-looping boot must never probe healthy).
	// Cleanup is deferred only after boot succeeds: a failed boot leaves
	// the unhealthy marker in place.
	marker := health.NewMarker(healthMarkerPath)
	// Clear a previous life's marker immediately: a docker restart preserves
	// /tmp, and until the mode-appropriate initial state is set after the
	// socket bind, a probe must not read the old life's healthy state
	// (built-in mode boots unhealthy by contract).
	marker.Set(false)

	if err := verifyBaseDir(ctx); err != nil {
		logBaseDirError(baseDir(), err)
		return err
	}

	interval, scheduleEnabled := loadInterval()
	timeout := loadRunTimeout()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// abort lets a fatal executor condition (containment loss) shut the
	// daemon down through the same context the SIGTERM path uses: the
	// ticker, the executor's queued-job cancellation, and the socket
	// handlers all key off this one ctx.
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

	ln, err := trigger.Listen(socketPath)
	if err != nil {
		slog.Error("cannot bind trigger socket", "path", socketPath, "error", err)
		return err
	}
	defer func() { _ = os.Remove(socketPath) }()

	defer marker.Cleanup()
	// Built-in mode starts unhealthy until the first run proves the setup
	// (the startup pass flips it); external mode starts healthy — idle,
	// nothing has failed — and each triggered run updates it.
	marker.Set(!scheduleEnabled)

	d := &daemon{
		queue:   trigger.NewQueue[runPayload](queueCapacity),
		marker:  marker,
		newCmd:  newCmd,
		runOnce: runOnce,
		runCtx:  context.WithoutCancel(ctx),
		timeout: timeout,
		fatal:   make(chan error, 1),
	}

	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		d.runJobs(ctx)
	}()

	// The broker owns the wire (decode, event relay, handler draining); the
	// hooks only supply this app's log vocabulary — the payload's repo scope,
	// never its environment (it can carry credentials).
	srv := &trigger.Server[runPayload]{
		Queue: d.queue,
		OnAccepted: func(p runPayload) {
			slog.Info("triggered run queued", "repos", p.Repos)
		},
		OnRejected: func(p runPayload, err error) {
			slog.Warn("trigger request rejected", "repos", p.Repos, "reason", err)
		},
	}
	srv.Serve(ln)

	tickerDone := startTicker(ctx, d, interval, scheduleEnabled)

	mode := "external"
	if scheduleEnabled {
		mode = "built-in"
	}
	slog.Info("container started",
		"mode", mode, "interval", interval, "timeout", timeout, "base_dir", baseDir(), "socket", socketPath)

	// Block until SIGTERM or the executor's fatal containment-loss signal;
	// the latter shuts down through the same abort context so the ticker,
	// admission, and handlers drain identically, but runDaemon returns the
	// error so main exits non-zero and the container restart reaps the
	// surviving process tree.
	var fatalErr error
	select {
	case <-ctx.Done():
	case fatalErr = <-d.fatal:
		abort(fatalErr)
	}
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Mark unhealthy immediately so observers see the signal before the run
	// drain (a Renovate run can take a while); beginShutdown also pins the
	// state so the draining run's completion cannot flip it back healthy.
	d.beginShutdown()

	// Stop admission (socket + queue), then wait: the executor finishes the
	// in-flight run uncancelled (runCtx) and delivers cancellation results to
	// everything still queued; the ticker returns once its waiting tick job
	// is resolved; the handlers return once every accepted request has its
	// final event on the wire.
	_ = ln.Close()
	d.queue.Close()
	<-executorDone
	// Fold in a late containment loss: if ordinary shutdown won the select
	// above while a run was still draining, the executor's fatal send landed
	// in the buffered channel after the receive was already passed. The
	// executor sends before runJobs can close executorDone, so this
	// non-blocking receive is ordered and cannot miss a loss from the
	// drained run.
	if fatalErr == nil {
		select {
		case fatalErr = <-d.fatal:
		default:
		}
	}
	<-tickerDone
	srv.Wait()
	slog.Info("shutdown complete")
	return fatalErr
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
			trig := "interval"
			if !startupDone {
				trig, startupDone = "startup", true
			}
			d.tick(trig)
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
func (d *daemon) tick(trig string) {
	j := newJob(trig, nil, nil)
	if err := d.queue.Submit(j); err != nil {
		slog.Warn("scheduled run skipped", "trigger", trig, "reason", err)
		return
	}
	<-j.Result()
}

// runJobs is the executor: the only code that starts Renovate. It serves the
// queue strictly in order until the queue is closed and drained. Once
// shutdown is signalled, remaining jobs are cancelled — delivered as explicit
// not-ok results with a reason — instead of run, so a stop request is never
// followed by a fresh run. Once containment is lost (a run's process group
// survived the kill sweep), remaining jobs are failed with the explicit
// containment reason instead of run: a surviving package-manager tree may
// still be writing the base directory, so no further run may be admitted.
func (d *daemon) runJobs(shutdownCtx context.Context) {
	for j := range d.queue.Jobs() {
		switch {
		case d.halted:
			cancelJobForContainment(j)
		case shutdownCtx.Err() != nil:
			cancelJobForShutdown(j, "queued")
		default:
			d.execute(shutdownCtx, j)
		}
	}
}

// shutdownCancelReason is the not-ok outcome reason a waiting trigger
// receives whenever shutdown cancels its job — still queued, caught by the
// post-preflight re-check, or reaped by runRenovateOnce's post-Start
// handshake.
const shutdownCancelReason = "cancelled: scheduler shutting down"

// cancelJobForShutdown delivers the explicit shutdown-cancellation result —
// the shared bookkeeping for both the already-shutting-down dequeue branch
// (stage "queued") and the post-preflight re-check in execute (stage
// "preflight", where the job's waiter has already seen a started event).
// The duration is always zero: a shutdown-cancelled job never starts
// Renovate, and the cancellation reason -- not elapsed queue or preflight
// time -- is the useful signal.
func cancelJobForShutdown(j *trigger.Job[runPayload], stage string) {
	slog.Warn("run cancelled by shutdown", "stage", stage, "trigger", j.Trigger, "repos", j.Payload.Repos)
	j.Finish(trigger.Outcome{OK: false, Reason: shutdownCancelReason})
}

// cancelJobForContainment fails a queued job after containment loss: the
// prior run's process group could not be confirmed dead, so running this job
// could overlap it against the same base directory. The waiter gets the
// explicit containment reason instead of a bare failure.
func cancelJobForContainment(j *trigger.Job[runPayload]) {
	slog.Warn("run cancelled: a prior run's process group survived the kill sweep",
		"trigger", j.Trigger, "repos", j.Payload.Repos)
	j.Finish(trigger.Outcome{OK: false, Reason: containmentLostReason})
}

// execute performs one job: signal the waiter, re-verify the base directory
// (a volume can degrade after boot; failing here beats a confusing Renovate
// error), run the pass, record the outcome on the health marker, and deliver
// the result. The base-dir preflight runs on the uncancelled runCtx (it can
// take up to 10s), so shutdownCtx is re-checked after it succeeds: a SIGTERM
// that lands during the preflight must cancel the job, never start a fresh
// Renovate pass after shutdown was requested. That re-check alone is a
// check-then-act race — a SIGTERM can still land between it and process
// creation — so shutdownCtx is also passed into runRenovateOnce, whose
// post-Start handshake reaps a child that started after shutdown won and
// reports it cancelled instead of committing it as in-flight.
func (d *daemon) execute(shutdownCtx context.Context, j *trigger.Job[runPayload]) {
	j.Start()
	start := time.Now()

	dir := baseDirForEnv(j.Payload.Env)
	if err := verifyBaseDirAt(d.runCtx, dir); err != nil {
		logBaseDirError(dir, err)
		d.setRunHealth(false)
		j.Finish(trigger.Outcome{OK: false, Duration: time.Since(start), Reason: "base directory not writable"})
		return
	}

	if shutdownCtx.Err() != nil {
		cancelJobForShutdown(j, "preflight")
		return
	}

	runOnce := d.runOnce
	if runOnce == nil {
		runOnce = runRenovateOnce
	}
	ok, cancelled, groupSurvived := runOnce(d.runCtx, shutdownCtx, d.timeout, j.Trigger, j.Payload.Repos, j.Payload.Env, d.newCmd)
	if cancelled {
		// runRenovateOnce already reaped the child and logged the Warn; the
		// health marker is left alone (beginShutdown pinned it unhealthy).
		j.Finish(trigger.Outcome{OK: false, Reason: shutdownCancelReason})
		return
	}
	if groupSurvived {
		// The kill sweep could not confirm the group's death: a
		// package-manager tree may still be writing the base directory.
		// Admitting another run would break the single-executor guarantee,
		// and a later clean run would flip the marker healthy while the
		// survivor keeps writing — so the executor halts. This job and every
		// remaining one fail with the explicit containment reason, and
		// runDaemon exits non-zero so the container restart reaps the
		// surviving tree.
		d.halted = true
		d.setRunHealth(false)
		slog.Error("halting run admission: renovate run process group survived the kill sweep",
			"trigger", j.Trigger)
		j.Finish(trigger.Outcome{OK: false, Duration: time.Since(start), Reason: containmentLostReason})
		d.fatal <- errContainmentLost
		return
	}
	d.setRunHealth(ok)
	j.Finish(trigger.Outcome{OK: ok, Duration: time.Since(start)})
}
