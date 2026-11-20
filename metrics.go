package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/measure/v2"
	"github.com/cplieger/go/scheduler/v2"
	"github.com/cplieger/go/scheduler/v2/trigger"
)

const metricsPrefix = "renovate_scheduler"

// version is the release this binary is, stamped by the Dockerfile's
// BUILD_VERSION build argument; an unstamped build reports "dev".
var version = "dev"

// Run scopes, the `scope` field of the run finished record and a label.
const (
	keyScope    = "scope"
	scopeFull   = "full"
	scopeScoped = "scoped"
)

const (
	rejectFull    = "full"
	rejectClosing = "closing"
)

var (
	runScopes  = []string{scopeFull, scopeScoped}
	runResults = []string{
		resultComplete, resultFailed, resultAborted, resultTimedOut, resultStartFailed,
		resultRepositoryError, resultTokenUnavailable, resultTokenFailed, resultPreflightFailed, resultContained,
	}
)

type runMetrics struct {
	runs         *measure.LabeledCounter
	lastOK       *measure.LabeledGauge
	lastResult   *measure.StateSet
	lastExitCode *measure.LabeledGauge
	lastRun      *measure.LabeledGauge
	lastSuccess  *measure.LabeledGauge
	lastDuration *measure.LabeledGauge
	failingSince *measure.LabeledGauge
	rejected     *measure.LabeledCounter
	info         *measure.Info
	deadline     *measure.Gauge
	next         *measure.Gauge
	gap          time.Duration
}

// runBoot is what the daemon knows when it builds the page.
type runBoot struct {
	start time.Time
	// last is the completed-run record; known is false when there is none.
	last scheduler.RunRecord
	// gap is how long after a complete full run the next one is due, the
	// health lease (runLease); 0 in external mode.
	gap   time.Duration
	known bool
}

// newRunMetrics registers the series on reg and seeds them: every counter
// label value at 0, both failure clocks at 0, the full scope's last success
// from boot.last, and the run deadline one boot.gap after that success, or
// after boot.start when there is none. The per-run series of a scope appear
// at its first run.
func newRunMetrics(reg *measure.Registry, mode, auth string, boot runBoot) *runMetrics {
	scope := []string{keyScope}
	scopeSeeds := make([][]string, 0, len(runScopes))
	runSeeds := make([][]string, 0, len(runScopes)*len(runResults))
	for _, s := range runScopes {
		scopeSeeds = append(scopeSeeds, []string{s})
		for _, r := range runResults {
			runSeeds = append(runSeeds, []string{s, r})
		}
	}
	m := &runMetrics{
		runs: measure.NewLabeledCounter("runs_total",
			"Completed runs by scope and how they ended.", measure.UnitCount, []string{keyScope, "result"},
			measure.WithSeed(runSeeds...)),
		lastOK: measure.NewLabeledGauge("last_run_ok",
			"1 when the scope's last completed run passed, a tolerated failure included, else 0.", measure.UnitNone, scope),
		lastResult: measure.NewStateSet("last_run_result",
			"1 for how the scope's last completed run ended, 0 for the other results.", []string{keyScope, "result"}, runResults),
		lastExitCode: measure.NewLabeledGauge("last_run_exit_code",
			"Renovate's exit code in the scope's last completed run; -1 when Renovate did not run or a signal ended it.", measure.UnitNone, scope),
		lastRun: measure.NewLabeledGauge("last_run_timestamp_seconds",
			"Unix time the scope's last completed run ended.", measure.UnitSeconds, scope),
		lastSuccess: measure.NewLabeledGauge("last_success_timestamp_seconds",
			"Unix time the scope's last complete run ended; for the full scope, read back from RENOVATE_BASE_DIR after a restart.", measure.UnitSeconds, scope),
		lastDuration: measure.NewLabeledGauge("last_run_duration_seconds",
			"How long the scope's last completed run took.", measure.UnitSeconds, scope),
		failingSince: measure.NewLabeledGauge("failing_since_timestamp_seconds",
			"Unix time the scope's current series of tolerable failures began; 0 when runs are not failing.", measure.UnitSeconds, scope,
			measure.WithSeed(scopeSeeds...)),
		rejected: measure.NewLabeledCounter("trigger_rejected_total",
			"Run requests the queue refused, because it was full or the daemon was shutting down.", measure.UnitCount, []string{"reason"},
			measure.WithSeed([]string{rejectFull}, []string{rejectClosing})),
		info: measure.NewInfo("info",
			"Always 1; the labels name the scheduling mode and the GitHub auth mode.", []string{"mode", "auth"}),
		deadline: measure.NewGauge("run_deadline_timestamp_seconds",
			"Unix time by which the next complete full run is due; 0 while RUN_INTERVAL is off.", measure.UnitSeconds),
		next: measure.NewGauge("next_run_timestamp_seconds",
			"Unix time the built-in schedule next starts a full run; 0 while RUN_INTERVAL is off.", measure.UnitSeconds),
		gap: boot.gap,
	}
	reg.MustRegister(m.runs, m.lastOK, m.lastResult, m.lastExitCode, m.lastRun, m.lastSuccess,
		m.lastDuration, m.failingSince, m.rejected, m.info, m.deadline, m.next)
	m.info.Set(mode, auth)
	lastSuccess := boot.start
	if boot.known && boot.last.OK {
		lastSuccess = boot.last.Time
		m.lastSuccess.Set(unixSeconds(lastSuccess), scopeFull)
	}
	m.setDeadline(lastSuccess)
	return m
}

// setNextRun records at as the time the built-in schedule next starts a full run.
func (m *runMetrics) setNextRun(at time.Time) {
	m.next.Set(unixSeconds(at))
}

// setDeadline dates the run deadline one gap after a complete full run at
// lastSuccess; with no gap it stays 0.
func (m *runMetrics) setDeadline(lastSuccess time.Time) {
	if m.gap > 0 {
		m.deadline.Set(unixSeconds(lastSuccess.Add(m.gap)))
	}
}

// newMetricsRegistry builds the registry the metrics page serves, seeded from
// the completed-run stamp, with the run deadline gap after the last complete
// full run. alert_parity_test.go builds it the same way.
func newMetricsRegistry(mode, auth string, completed *scheduler.Stamp, gap time.Duration) (*measure.Registry, *runMetrics) {
	reg := measure.NewRegistry(metricsPrefix)
	registerBuildInfo(reg)
	last, known := completed.Last()
	return reg, newRunMetrics(reg, mode, auth, runBoot{start: time.Now(), last: last, known: known, gap: gap})
}

// registerBuildInfo serves build_info at 1, labelled with the release this
// process runs, for a rule to join on whatever the target's scrape job is.
func registerBuildInfo(reg *measure.Registry) {
	info := measure.NewInfo("build_info",
		"Always 1; the version label is the release this process runs.", []string{"version"})
	reg.MustRegister(info)
	info.Set(version)
}

func unixSeconds(t time.Time) float64 {
	return float64(t.Unix())
}

func (m *runMetrics) observe(scope string, v runVerdict, elapsed time.Duration, at, failingSince time.Time) {
	m.runs.Add(1, scope, v.result)
	m.lastResult.Set(v.result, scope)
	m.lastOK.Set(float64(boolDigit(v.ok)), scope)
	m.lastExitCode.Set(float64(v.exitCode), scope)
	m.lastRun.Set(unixSeconds(at), scope)
	m.lastDuration.Set(elapsed.Seconds(), scope)
	if v.result == resultComplete {
		m.lastSuccess.Set(unixSeconds(at), scope)
		if scope == scopeFull {
			m.setDeadline(at)
		}
	}
	clock := 0.0
	if !failingSince.IsZero() {
		clock = unixSeconds(failingSince)
	}
	m.failingSince.Set(clock, scope)
}

func (m *runMetrics) rejectedRun(err error) {
	reason := rejectClosing
	if errors.Is(err, trigger.ErrFull) {
		reason = rejectFull
	}
	m.rejected.Add(1, reason)
}

// serveOps binds the ops listener on addr and serves reg's page and healthz on
// it until ctx ends or stop is called; stop shuts the server down and waits for
// it. An empty addr opens no listener; a bind or host-list failure is returned
// and nothing is served.
func serveOps(ctx context.Context, addr string, hosts []string, reg *measure.Registry, healthz http.Handler) (stop func(), err error) {
	ops, err := httpserver.ListenOps(ctx, httpserver.OpsConfig{
		Metrics:      reg.Handler(),
		Healthz:      healthz,
		Setting:      "METRICS_LISTEN_ADDR",
		Addr:         addr,
		HostsSetting: "ALLOWED_HOSTS",
		Hosts:        hosts,
	})
	if err != nil {
		return nil, err
	}
	bound := ops.Addr()
	if bound == nil {
		return func() {}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := httpserver.RunWithOps(ctx, ops); err != nil {
			slog.Error("ops listener stopped", "address", addr, "error", err)
		}
	}()
	slog.Info("ops listener started", "address", bound.String())
	return func() {
		cancel()
		<-done
	}, nil
}
