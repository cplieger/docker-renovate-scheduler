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

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/scheduler/trigger"
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
	// completed records every complete full run, to seed metrics after a restart.
	completed *scheduler.Stamp
	metrics   *runMetrics
	tokens    *appTokenMinter // nil: Renovate uses its own RENOVATE_TOKEN
	newCmd    scheduler.CommandRunner
	runOnce   func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult
	now       func() time.Time // a run's start time
	fatal     chan error
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
		return daemonStopped(err)
	}

	ln, err := trigger.Listen(socketPath)
	if err != nil {
		slog.Error("cannot bind trigger socket", "path", socketPath, "error", err)
		return daemonStopped(err)
	}

	marker := health.NewMarker(healthMarkerPath)
	marker.Set(false)

	verifier := newBaseDirVerifier()
	if err = verifier.verify(ctx); err != nil {
		logBaseDirError(baseDir(), err)
		_ = ln.Close()
		return daemonStopped(err)
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

	completed := scheduler.NewStamp(completedStampPath())
	reg, metrics := newMetricsRegistry(marker, modeName(scheduleEnabled), authName(app), completed)
	metricsAddr := metricsListenAddr()
	stopMetrics, err := serveMetrics(ctx, metricsAddr, reg)
	if err != nil {
		slog.Error("cannot listen on METRICS_LISTEN_ADDR", "value", metricsAddr, "error", err)
		_ = ln.Close()
		return daemonStopped(err)
	}
	defer stopMetrics()

	d := &daemon{
		queue:     trigger.NewQueue[runPayload](queueCapacity),
		marker:    marker,
		health:    health.NewLatch(marker),
		verifier:  verifier,
		stamp:     stamp,
		completed: completed,
		metrics:   metrics,
		tokens:    newAppTokenMinter(app),
		newCmd:    newCmd,
		runOnce:   runRenovateOnce,
		now:       time.Now,
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
			d.metrics.rejectedRun(err)
			slog.Warn("trigger request rejected", "repos", p.Repos, "reason", err)
		},
	}
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx, ln) }()

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
	<-served
	slog.Info("shutdown complete")
	if fatalErr != nil {
		return daemonStopped(fatalErr)
	}
	return nil
}

// daemonStopped writes the one line that says the daemon is exiting
// abnormally, at every error return of runDaemon, and returns err.
func daemonStopped(err error) error {
	slog.Error(daemonStoppedMsg, "error", err)
	return err
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
		d.metrics.rejectedRun(err)
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
			j.Finish(trigger.Result{OK: false, Reason: containmentLostReason})
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
	j.Finish(trigger.Result{OK: false, Duration: elapsed, Reason: shutdownCancelReason})
}

func (d *daemon) execute(runCtx context.Context, stopping func() error, j *trigger.Job[runPayload]) {
	j.Start()
	start := d.now()

	dir := baseDirForEnv(j.Payload.Env)
	if err := d.verifier.verifyAt(runCtx, dir); err != nil {
		logBaseDirError(dir, err)
		// The stamp shares the probed directory, so a timeout cannot safely write it.
		if !errors.Is(err, context.DeadlineExceeded) {
			d.recordScheduled(j.Trigger, false)
		}
		d.finish(j, start, runVerdict{result: resultPreflightFailed, reason: "base directory preflight failed", exitCode: noExitCode})
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

	res := d.runOnce(runCtx, d.timeout, j.Trigger, p, d.newCmd)
	if res.outcome == runContained {
		d.halted = true
		// A contained scheduled run leaves the next boot due.
		d.recordScheduled(j.Trigger, false)
		slog.Error("halting run admission: renovate run process group survived the kill sweep",
			"trigger", j.Trigger)
		d.finish(j, start, runVerdict{result: resultContained, reason: containmentLostReason, exitCode: res.exitCode})
		d.fatal <- errContainmentLost
		return
	}
	ok := res.outcome == runComplete
	tolerated := false
	if res.outcome == runRepositoryError {
		var failing time.Duration
		tolerated, failing = d.tolerate(j, start)
		logRepositoryError(runCtx, j, res, failing, tolerated)
	}
	if ok {
		d.recovered(j)
	}
	// The stamp keeps the real result, so a restart during an outage retries at boot.
	d.recordScheduled(j.Trigger, ok)
	reason := failureReason(res.outcome, d.timeout)
	if tolerated {
		reason = toleratedReason
	}
	d.finish(j, start, runVerdict{
		result: resultOf(res), reason: reason, exitCode: res.exitCode, ok: ok || tolerated, tolerated: tolerated,
	})
}

// Completed-run results, the `result` field of the run finished record.
const (
	resultComplete         = "complete"
	resultFailed           = "failed"
	resultAborted          = "aborted"
	resultTimedOut         = "timed_out"
	resultStartFailed      = "start_failed"
	resultRepositoryError  = "repository_error"
	resultTokenUnavailable = "token_unavailable" //nolint:gosec // a result name, not a credential
	resultTokenFailed      = "token_failed"
	resultPreflightFailed  = "preflight_failed"
	resultContained        = "contained"

	runFinishedMsg   = "run finished"
	daemonStoppedMsg = "daemon stopped"
	// noExitCode is the record's exit_code when Renovate did not run.
	noExitCode = -1
)

func resultOf(res runResult) string {
	switch res.outcome {
	case runComplete:
		return resultComplete
	case runStartFailed:
		return resultStartFailed
	case runTimedOut:
		return resultTimedOut
	case runRepositoryError:
		return resultRepositoryError
	case runContained:
		return resultContained
	}
	if res.exitCode == abortExitCode {
		return resultAborted
	}
	return resultFailed
}

// runVerdict is how a completed run ended. ok is the verdict the run client
// reports, so a tolerated failure is ok.
type runVerdict struct {
	result    string
	reason    string
	exitCode  int
	ok        bool
	tolerated bool
}

// finish ends every completed run: it sets health and the metrics from the
// verdict, writes the run's one run finished record and finishes the job, so
// the record, the metrics, the health marker and the run client's exit code
// cannot disagree. A tolerated failure leaves health as it was (keepHealth).
func (d *daemon) finish(j *trigger.Job[runPayload], start time.Time, v runVerdict) {
	if v.tolerated {
		d.keepHealth()
	} else {
		d.health.Set(v.ok)
	}
	at := time.Now()
	elapsed := at.Sub(start)
	scope := scopeOf(j)
	if v.result == resultComplete && scope == scopeFull {
		d.recordComplete()
	}
	d.metrics.observe(scope, v, elapsed, at, *d.failingSinceFor(j))
	slog.Info(runFinishedMsg, "trigger", j.Trigger, keyScope, scope, "result", v.result,
		"ok", boolDigit(v.ok), "tolerated", boolDigit(v.tolerated),
		"exit_code", v.exitCode, "duration_ms", elapsed.Milliseconds())
	j.Finish(trigger.Result{OK: v.ok, Duration: elapsed, Reason: v.reason})
}

func scopeOf(j *trigger.Job[runPayload]) string {
	if len(j.Payload.Repos) > 0 {
		return scopeScoped
	}
	return scopeFull
}

func modeName(scheduleEnabled bool) string {
	if scheduleEnabled {
		return "built-in"
	}
	return "external"
}

func authName(app *appConfig) string {
	if app == nil {
		return "token"
	}
	return "app"
}

// recordComplete persists a complete full run, so the last-success series
// starts from it after a restart. Failures are not recorded: they would
// erase the last success the record seeds.
func (d *daemon) recordComplete() {
	if err := d.completed.Record(true); err != nil {
		slog.Warn("cannot record the complete run; the last-success metric restarts at 0",
			"path", completedStampPath(), "error", err)
	}
}

// boolDigit writes a verdict as 1 or 0, because a log rule unwraps it as a number.
func boolDigit(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (d *daemon) failingSinceFor(j *trigger.Job[runPayload]) *time.Time {
	if len(j.Payload.Repos) > 0 {
		return &d.scopedFailingSince
	}
	return &d.failingSince
}

// tolerate reports whether a failure an outage can cause passes as a warning
// (the first of a series, and any within failureGrace of that run's start)
// and how long runs have failed. A tolerated failure keeps health's state
// (keepHealth).
func (d *daemon) tolerate(j *trigger.Job[runPayload], start time.Time) (tolerated bool, failingFor time.Duration) {
	since := d.failingSinceFor(j)
	first := since.IsZero()
	if first {
		*since = start
	}
	failing := time.Since(*since)
	return first || failing < failureGrace, failing.Round(time.Second)
}

// failureLevel is the level of a failed run's one line: WARN while the
// failure is tolerated, ERROR once it fails the run.
func failureLevel(tolerated bool) slog.Level {
	if tolerated {
		return slog.LevelWarn
	}
	return slog.LevelError
}

func logRepositoryError(ctx context.Context, j *trigger.Job[runPayload], res runResult, failing time.Duration, tolerated bool) {
	diag, _ := repositoryErrorDiagnosis(res.err)
	slog.Log(ctx, failureLevel(tolerated), "renovate run ended in a repository error",
		"trigger", j.Trigger, "duration_ms", res.duration.Milliseconds(), "error", res.err,
		"likely_cause", diag.cause, "fix", diag.fix, "failing_for", failing)
}

// recovered ends the job's series of tolerable failures at a good run.
func (d *daemon) recovered(j *trigger.Job[runPayload]) {
	since := d.failingSinceFor(j)
	if since.IsZero() {
		return
	}
	slog.Info("renovate runs recovered", "trigger", j.Trigger, keyScope, scopeOf(j),
		"failing_for", time.Since(*since).Round(time.Second))
	*since = time.Time{}
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
		d.recordScheduled(j.Trigger, false)
		if !transientMintError(err) {
			slog.Error("github app token request failed", "trigger", j.Trigger, "error", err)
			d.finish(j, start, runVerdict{result: resultTokenFailed, reason: mintFailedReason, exitCode: noExitCode})
			return p, false
		}
		tolerated, failing := d.tolerate(j, start)
		slog.Log(ctx, failureLevel(tolerated), "renovate run skipped: github app token unavailable",
			"trigger", j.Trigger, "error", err, "failing_for", failing)
		v := runVerdict{result: resultTokenUnavailable, reason: mintFailedReason, exitCode: noExitCode}
		if tolerated {
			v.reason, v.ok, v.tolerated = toleratedReason, true, true
		}
		d.finish(j, start, v)
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
