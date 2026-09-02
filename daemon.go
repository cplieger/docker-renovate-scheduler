package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/scheduler/v4/trigger"
)

var errContainmentLost = errors.New("renovate run process group survived the kill sweep")

const (
	containmentLostReason = "failed: run process group survived the kill sweep; scheduler halting"
	queueCapacity         = 16
)

type runOnceFunc func(ctx context.Context, stopping stopRequested, timeout time.Duration, trig string, p runPayload, newCmd scheduler.CommandRunner) runOutcome

type daemon struct {
	queue     *trigger.Queue[runPayload]
	marker    *health.Marker
	health    *health.Latch
	verifier  *baseDirVerifier
	stamp     *scheduler.Stamp
	newCmd    scheduler.CommandRunner
	runOnce   runOnceFunc
	fatal     chan error
	stampPath string
	timeout   time.Duration
	// Only the executor accesses halted.
	halted bool
}

func runDaemon(ctx context.Context, socketPath string, newCmd scheduler.CommandRunner) error {
	warnIfRootlessCacheUnwritable()

	marker := health.NewMarker(healthMarkerPath)
	marker.Set(false)

	verifier := newBaseDirVerifier()
	if err := verifier.verify(ctx); err != nil {
		logBaseDirError(baseDir(), err)
		return err
	}

	interval, scheduleEnabled := loadInterval()
	timeout := loadRunTimeout()

	stamp := scheduler.NewStamp(stampPath())
	due := true
	if scheduleEnabled {
		due = stamp.Due(interval, time.Now(), scheduler.RetryFailed)
	}

	ln, err := trigger.Listen(socketPath)
	if err != nil {
		slog.Error("cannot bind trigger socket", "path", socketPath, "error", err)
		return err
	}

	d := &daemon{
		queue:     trigger.NewQueue[runPayload](queueCapacity),
		marker:    marker,
		health:    health.NewLatch(marker),
		verifier:  verifier,
		stamp:     stamp,
		newCmd:    newCmd,
		stampPath: stampPath(),
		timeout:   timeout,
		fatal:     make(chan error, 1),
	}
	return d.run(ctx, ln, socketPath, interval, scheduleEnabled, due)
}

func (d *daemon) run(ctx context.Context, ln net.Listener, socketPath string, interval time.Duration, scheduleEnabled, due bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

	defer func() { _ = os.Remove(socketPath) }()
	defer d.marker.Cleanup()
	// Not due means a fresh successful run survived the restart, so booting healthy is honest.
	d.health.Set(!scheduleEnabled || !due)

	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		d.runJobs(ctx)
	}()

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

	tickerDone := startTicker(ctx, d, interval, scheduleEnabled, due)

	if scheduleEnabled {
		slog.Info("container started", "mode", "built-in", "interval", interval,
			"timeout", d.timeout, "base_dir", baseDir(), "socket", socketPath,
			"startup_run", due)
		if !due {
			rec, _ := d.stamp.Last()
			slog.Info("startup run skipped: the last scheduled run succeeded within the interval",
				"last_success", rec.Time, "interval", interval)
		}
	} else {
		slog.Info("container started", "mode", "external",
			"timeout", d.timeout, "base_dir", baseDir(), "socket", socketPath)
	}

	var fatalErr error
	select {
	case <-ctx.Done():
	case fatalErr = <-d.fatal:
		abort(fatalErr)
	}
	slog.Info("shutting down", "cause", context.Cause(ctx))
	d.health.BeginDrain()

	_ = ln.Close()
	d.queue.Close()
	<-executorDone
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

func startTicker(ctx context.Context, d *daemon, interval time.Duration, enabled, due bool) <-chan struct{} {
	done := make(chan struct{})
	if !enabled {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		if due && ctx.Err() == nil {
			d.tick("startup")
		}
		scheduler.RunLoop(ctx, func(context.Context) {
			d.tick("interval")
		}, scheduler.LoopOptions{
			Interval:    interval,
			FireOnStart: false,
			// Phase the first tick from the recorded previous run, so a
			// restart neither adds a run nor delays the cadence.
			FirstDelay: d.stamp.Remaining(interval, time.Now(), scheduler.RetryFailed),
		})
	}()
	return done
}

func (d *daemon) tick(trig string) {
	j := trigger.NewJob(trig, runPayload{})
	if err := d.queue.Submit(j); err != nil {
		slog.Warn("scheduled run skipped", "trigger", trig, "reason", err)
		return
	}
	<-j.Result()
}

func (d *daemon) runJobs(shutdownCtx context.Context) {
	// An in-flight run drains after shutdown, bounded by RUN_TIMEOUT.
	runCtx := context.WithoutCancel(shutdownCtx)
	for j := range d.queue.Jobs() {
		switch {
		case d.halted:
			cancelJobForContainment(j)
		case shutdownCtx.Err() != nil:
			cancelJobForShutdown(j, "queued")
		default:
			d.execute(runCtx, shutdownCtx.Err, j)
		}
	}
}

const shutdownCancelReason = trigger.CancelledReason

func cancelJobForShutdown(j *trigger.Job[runPayload], stage string) {
	slog.Warn("run cancelled by shutdown", "stage", stage, "trigger", j.Trigger, "repos", j.Payload.Repos)
	j.Finish(trigger.Outcome{OK: false, Reason: shutdownCancelReason})
}

func cancelJobForContainment(j *trigger.Job[runPayload]) {
	slog.Warn("run cancelled: a prior run's process group survived the kill sweep",
		"trigger", j.Trigger, "repos", j.Payload.Repos)
	j.Finish(trigger.Outcome{OK: false, Reason: containmentLostReason})
}

func (d *daemon) execute(runCtx context.Context, stopping stopRequested, j *trigger.Job[runPayload]) {
	j.Start()
	start := time.Now()

	dir := baseDirForEnv(j.Payload.Env)
	if err := d.verifier.verifyAt(runCtx, dir); err != nil {
		logBaseDirError(dir, err)
		d.health.Set(false)
		j.Finish(trigger.Outcome{OK: false, Duration: time.Since(start), Reason: "base directory preflight failed"})
		return
	}

	if stopping() != nil {
		cancelJobForShutdown(j, "preflight")
		return
	}

	runOnce := d.runOnce
	if runOnce == nil {
		runOnce = runRenovateOnce
	}
	outcome := runOnce(runCtx, stopping, d.timeout, j.Trigger, j.Payload, d.newCmd)
	if outcome == runCancelled {
		j.Finish(trigger.Outcome{OK: false, Reason: shutdownCancelReason})
		return
	}
	if outcome == runContained {
		d.halted = true
		d.health.Set(false)
		// After a halt the next boot must treat the schedule as due.
		d.recordScheduled(j.Trigger, false)
		slog.Error("halting run admission: renovate run process group survived the kill sweep",
			"trigger", j.Trigger)
		j.Finish(trigger.Outcome{OK: false, Duration: time.Since(start), Reason: containmentLostReason})
		d.fatal <- errContainmentLost
		return
	}
	ok := outcome == runComplete
	d.health.Set(ok)
	d.recordScheduled(j.Trigger, ok)
	j.Finish(trigger.Outcome{OK: ok, Duration: time.Since(start)})
}

// recordScheduled persists a scheduled run's outcome for the next boot's
// startup-fire decision. Triggered runs are scoped to their own repos and
// environment, so they never answer the full-pass freshness question.
func (d *daemon) recordScheduled(trig string, ok bool) {
	if trig != "startup" && trig != "interval" {
		return
	}
	if err := d.stamp.Record(ok); err != nil {
		slog.Warn("cannot record the run outcome; next boot fires a startup run",
			"path", d.stampPath, "error", err)
	}
}
