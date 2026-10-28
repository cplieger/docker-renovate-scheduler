package main

import (
	"context"
	"errors"
	"fmt"
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
	// failureGrace is how long runs may keep failing in a way a platform outage
	// causes before one fails: past the median GitHub incident (about 80 min in
	// its 2024-2026 availability reports) and Renovate's 2-hour lockfile-error
	// hold after a release. With runs every hour, the third failing run is
	// usually the first to fail.
	failureGrace    = 2 * time.Hour
	toleratedReason = "ok: tolerated a repository error or token outage; runs fail once one lasts 2h (see the container log stream)"
	escalatedReason = "failed: runs have failed for over 2h (see the container log stream)"
)

type daemon struct {
	queue    *trigger.Queue[runPayload]
	marker   *health.Marker
	health   *health.Latch
	verifier *baseDirVerifier
	stamp    *scheduler.Stamp
	tokens   *appTokenMinter // nil: Renovate uses its own RENOVATE_TOKEN
	newCmd   scheduler.CommandRunner
	runOnce  func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome
	fatal    chan error
	// Only the executor accesses the failing-since times and halted. Each is the
	// start of the first run in an unbroken series of tolerable failures, zero
	// after a good run. Full passes and scoped runs keep their own, so the
	// frequent scoped runs a webhook sends cannot reset a fault only the full
	// pass meets.
	failingSince       time.Time
	scopedFailingSince time.Time
	stampPath          string
	timeout            time.Duration
	halted             bool
}

func runDaemon(ctx context.Context, socketPath string, newCmd scheduler.CommandRunner) error {
	warnIfRootlessCacheUnwritable()

	app, err := loadAppConfig()
	if err != nil {
		slog.Error("github app configuration invalid", "error", err,
			"hint", "see docs/configuration.md, 'GitHub App authentication'")
		return err
	}

	ln, err := trigger.Listen(socketPath)
	if err != nil {
		slog.Error("cannot bind trigger socket", "path", socketPath, "error", err)
		return err
	}

	marker := health.NewMarker(healthMarkerPath)
	marker.Set(false)

	verifier := newBaseDirVerifier()
	if err = verifier.verify(ctx); err != nil {
		logBaseDirError(baseDir(), err)
		_ = ln.Close()
		return err
	}

	interval, scheduleEnabled := loadInterval()
	runTimeout := loadRunTimeout()
	timeout := runTimeoutFor(app, runTimeout)

	stamp := scheduler.NewStamp(stampPath())
	var remaining time.Duration
	if scheduleEnabled {
		remaining = stamp.Remaining(interval, time.Now(), scheduler.RetryFailed)
	}

	logAuthMode(app, runTimeout, timeout)

	d := &daemon{
		queue:     trigger.NewQueue[runPayload](queueCapacity),
		marker:    marker,
		health:    health.NewLatch(marker),
		verifier:  verifier,
		stamp:     stamp,
		tokens:    newAppTokenMinter(app),
		newCmd:    newCmd,
		runOnce:   runRenovateOnce,
		stampPath: stampPath(),
		timeout:   timeout,
		fatal:     make(chan error, 1),
	}
	return d.run(ctx, ln, socketPath, interval, scheduleEnabled, remaining)
}

func (d *daemon) run(ctx context.Context, ln net.Listener, socketPath string, interval time.Duration, scheduleEnabled bool, remaining time.Duration) error {
	due := remaining == 0
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

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

	tickerDone := startTicker(ctx, d, interval, scheduleEnabled, remaining)

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

func startTicker(ctx context.Context, d *daemon, interval time.Duration, enabled bool, remaining time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if !enabled {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		if remaining == 0 {
			d.tick("startup")
		}
		scheduler.RunLoop(ctx, func(context.Context) {
			d.tick("interval")
		}, scheduler.LoopOptions{
			Interval:    interval,
			FireOnStart: false,
			// Phase the first tick from the recorded previous run, so a
			// restart neither adds a run nor delays the cadence.
			FirstDelay: remaining,
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
			slog.Warn("run cancelled: a prior run's process group survived the kill sweep",
				"trigger", j.Trigger, "repos", j.Payload.Repos)
			j.Finish(trigger.Outcome{OK: false, Reason: containmentLostReason})
		case shutdownCtx.Err() != nil:
			cancelJobForShutdown(j, "queued", 0)
		default:
			d.execute(runCtx, shutdownCtx.Err, j)
		}
	}
}

const shutdownCancelReason = trigger.CancelledReason

func cancelJobForShutdown(j *trigger.Job[runPayload], stage string, elapsed time.Duration) {
	slog.Warn("run cancelled by shutdown", "stage", stage, "trigger", j.Trigger, "repos", j.Payload.Repos)
	j.Finish(trigger.Outcome{OK: false, Duration: elapsed, Reason: shutdownCancelReason})
}

func (d *daemon) execute(runCtx context.Context, stopping func() error, j *trigger.Job[runPayload]) {
	j.Start()
	start := time.Now()

	dir := baseDirForEnv(j.Payload.Env)
	if err := d.verifier.verifyAt(runCtx, dir); err != nil {
		logBaseDirError(dir, err)
		d.health.Set(false)
		// The stamp shares the probed directory, so a timeout cannot safely write it.
		if !errors.Is(err, context.DeadlineExceeded) {
			d.recordScheduled(j.Trigger, false)
		}
		j.Finish(trigger.Outcome{OK: false, Duration: time.Since(start), Reason: "base directory preflight failed"})
		return
	}

	p, authorized := d.authorize(runCtx, j, start)
	if !authorized {
		return
	}

	if stopping() != nil {
		cancelJobForShutdown(j, "preflight", time.Since(start))
		return
	}

	outcome := d.runOnce(runCtx, d.timeout, j.Trigger, p, d.newCmd)
	if outcome == runContained {
		d.halted = true
		d.health.Set(false)
		// A contained scheduled run leaves the next boot due.
		d.recordScheduled(j.Trigger, false)
		slog.Error("halting run admission: renovate run process group survived the kill sweep",
			"trigger", j.Trigger)
		j.Finish(trigger.Outcome{OK: false, Duration: time.Since(start), Reason: containmentLostReason})
		d.fatal <- errContainmentLost
		return
	}
	ok := outcome == runComplete
	tolerated := outcome == runRepositoryError && d.tolerate(j, start)
	switch {
	case ok:
		*d.failingSinceFor(j) = time.Time{}
		d.health.Set(true)
	case tolerated:
		d.keepHealth()
	default:
		d.health.Set(false)
	}
	// The stamp keeps the real result, so a restart during an outage retries at boot.
	d.recordScheduled(j.Trigger, ok)
	reason := failureReason(outcome, d.timeout)
	if tolerated {
		reason = toleratedReason
	}
	j.Finish(trigger.Outcome{OK: ok || tolerated, Duration: time.Since(start), Reason: reason})
}

func (d *daemon) failingSinceFor(j *trigger.Job[runPayload]) *time.Time {
	if len(j.Payload.Repos) > 0 {
		return &d.scopedFailingSince
	}
	return &d.failingSince
}

// tolerate reports whether a failure an outage can cause passes as a warning:
// the first of a series, and any within failureGrace of that run's start. A
// tolerated failure keeps health's state (keepHealth). Past the grace it logs
// the escalation and returns false.
func (d *daemon) tolerate(j *trigger.Job[runPayload], start time.Time) bool {
	since := d.failingSinceFor(j)
	if since.IsZero() {
		*since = start
		return true
	}
	failing := time.Since(*since)
	if failing < failureGrace {
		return true
	}
	slog.Error("renovate runs keep failing", "trigger", j.Trigger, "repos", j.Payload.Repos,
		"failing_for", failing.Round(time.Second))
	return false
}

// keepHealth leaves health as it was after a tolerated failure, refreshing a
// healthy marker: a run did happen, so the built-in lease must not lapse
// during an outage shorter than failureGrace.
func (d *daemon) keepHealth() {
	if d.marker.Healthy() {
		d.health.Set(true)
	}
}

const mintFailedReason = "failed: no GitHub App installation token (see the container log stream)"

// authorize returns the job's payload, carrying a fresh installation token in
// App mode. On a mint failure it finishes the job as failed and returns false.
func (d *daemon) authorize(ctx context.Context, j *trigger.Job[runPayload], start time.Time) (runPayload, bool) {
	p := j.Payload
	if d.tokens == nil {
		return p, true
	}
	tok, err := d.tokens.mint(ctx)
	if err != nil {
		if transientMintError(err) && d.tolerate(j, start) {
			slog.Warn("renovate run skipped: github app token unavailable", "trigger", j.Trigger, "error", err)
			d.keepHealth()
			d.recordScheduled(j.Trigger, false)
			j.Finish(trigger.Outcome{OK: true, Duration: time.Since(start), Reason: toleratedReason})
			return p, false
		}
		slog.Error("github app token request failed", "trigger", j.Trigger, "error", err)
		d.health.Set(false)
		d.recordScheduled(j.Trigger, false)
		j.Finish(trigger.Outcome{OK: false, Duration: time.Since(start), Reason: mintFailedReason})
		return p, false
	}
	slog.Info("github app installation token issued", "trigger", j.Trigger,
		"installation_id", d.tokens.installationID, "expires_at", tok.expiresAt)
	p.Env = withRenovateToken(p.Env, tok.value)
	return p, true
}

func failureReason(outcome runOutcome, timeout time.Duration) string {
	switch outcome {
	case runStartFailed:
		return "failed: renovate could not be started (see the container log stream)"
	case runTimedOut:
		return fmt.Sprintf("failed: run exceeded RUN_TIMEOUT (%s)", timeout)
	case runRepositoryError:
		return escalatedReason
	default:
		return ""
	}
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
