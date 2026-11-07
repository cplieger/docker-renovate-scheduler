package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/scheduler/trigger"
)

// metricsPrefix names every series on the metrics page.
const metricsPrefix = "renovate_scheduler"

// Run scopes, the `scope` field of the run finished record and a label.
const (
	keyScope    = "scope"
	scopeFull   = "full"
	scopeScoped = "scoped"
)

// Why the queue refused a run, the reason label of the rejection counter.
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

// runMetrics are the series each completed run sets, from the values its run
// finished record carries.
type runMetrics struct {
	runs         *measure.LabeledCounter
	lastOK       *measure.LabeledGauge
	lastResult   *measure.LabeledGauge
	lastExitCode *measure.LabeledGauge
	lastRun      *measure.LabeledGauge
	lastSuccess  *measure.LabeledGauge
	lastDuration *measure.LabeledGauge
	failingSince *measure.LabeledGauge
	rejected     *measure.LabeledCounter
	info         *measure.LabeledGauge
}

// newRunMetrics registers the series on reg and seeds them: every counter
// label value at 0, both failure clocks at 0, and the full scope's last
// success from last, the record of the last complete full run that survived
// a restart. The per-run series of a scope appear at its first run.
func newRunMetrics(reg *measure.Registry, marker *health.Marker, mode, auth string, last scheduler.RunRecord, known bool) *runMetrics {
	scope := []string{keyScope}
	m := &runMetrics{
		runs: measure.NewLabeledCounter("runs_total",
			"Completed runs by scope and how they ended.", []string{keyScope, "result"}),
		lastOK: measure.NewLabeledGauge("last_run_ok",
			"1 when the scope's last completed run passed, a tolerated failure included, else 0.", scope),
		lastResult: measure.NewLabeledGauge("last_run_result",
			"1 for how the scope's last completed run ended, 0 for the other results.", []string{keyScope, "result"}),
		lastExitCode: measure.NewLabeledGauge("last_run_exit_code",
			"Renovate's exit code in the scope's last completed run; -1 when Renovate did not run or a signal ended it.", scope),
		lastRun: measure.NewLabeledGauge("last_run_timestamp_seconds",
			"Unix time the scope's last completed run ended.", scope),
		lastSuccess: measure.NewLabeledGauge("last_success_timestamp_seconds",
			"Unix time the scope's last complete run ended; for the full scope, read back from RENOVATE_BASE_DIR after a restart.", scope),
		lastDuration: measure.NewLabeledGauge("last_run_duration_seconds",
			"How long the scope's last completed run took.", scope),
		failingSince: measure.NewLabeledGauge("failing_since_timestamp_seconds",
			"Unix time the scope's current series of tolerable failures began; 0 when runs are not failing.", scope),
		rejected: measure.NewLabeledCounter("trigger_rejected_total",
			"Run requests the queue refused, because it was full or the daemon was shutting down.", []string{"reason"}),
		info: measure.NewLabeledGauge("info",
			"Always 1; the labels name the scheduling mode and the GitHub auth mode.", []string{"mode", "auth"}),
	}
	healthyDesc := measure.NewGaugeDesc("healthy",
		"1 while the health marker the Docker healthcheck reads is set, else 0.", nil)
	healthy := measure.NewCollector(func(_ context.Context, e *measure.Emitter) {
		e.Emit(healthyDesc, float64(boolDigit(marker.Healthy())))
	}, healthyDesc)
	reg.MustRegister(m.runs, m.lastOK, m.lastResult, m.lastExitCode, m.lastRun, m.lastSuccess,
		m.lastDuration, m.failingSince, m.rejected, m.info, healthy)
	for _, s := range runScopes {
		for _, r := range runResults {
			m.runs.Add(0, s, r)
		}
		m.failingSince.Set(0, s)
	}
	m.rejected.Add(0, rejectFull)
	m.rejected.Add(0, rejectClosing)
	m.info.Set(1, mode, auth)
	if known {
		m.lastSuccess.Set(unixSeconds(last.Time), scopeFull)
	}
	return m
}

func unixSeconds(t time.Time) float64 {
	return float64(t.Unix())
}

// observe sets a scope's series from one completed run.
func (m *runMetrics) observe(scope string, v runVerdict, elapsed time.Duration, at, failingSince time.Time) {
	m.runs.Add(1, scope, v.result)
	for _, r := range runResults {
		m.lastResult.Set(float64(boolDigit(r == v.result)), scope, r)
	}
	m.lastOK.Set(float64(boolDigit(v.ok)), scope)
	m.lastExitCode.Set(float64(v.exitCode), scope)
	m.lastRun.Set(unixSeconds(at), scope)
	m.lastDuration.Set(elapsed.Seconds(), scope)
	if v.result == resultComplete {
		m.lastSuccess.Set(unixSeconds(at), scope)
	}
	clock := 0.0
	if !failingSince.IsZero() {
		clock = unixSeconds(failingSince)
	}
	m.failingSince.Set(clock, scope)
}

// rejectedRun counts one run request the queue refused with err.
func (m *runMetrics) rejectedRun(err error) {
	reason := rejectClosing
	if errors.Is(err, trigger.ErrFull) {
		reason = rejectFull
	}
	m.rejected.Add(1, reason)
}

// listenTCP is a seam so a test can see that an unset address opens nothing.
var listenTCP = net.Listen

// Timeouts for the metrics page: a scrape reads one small page, so a slow
// client is cut off rather than held.
const (
	metricsReadTimeout  = 10 * time.Second
	metricsWriteTimeout = 30 * time.Second
)

// serveMetrics binds addr and serves reg's page on it until ctx ends or stop
// is called; stop shuts the server down and waits for it. An empty addr opens
// no listener; a bind failure is returned and nothing is served.
func serveMetrics(ctx context.Context, addr string, reg *measure.Registry) (stop func(), err error) {
	if addr == "" {
		return func() {}, nil
	}
	ln, err := listenTCP("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	srv := httpserver.New(mux,
		httpserver.WithReadTimeout(metricsReadTimeout),
		httpserver.WithWriteTimeout(metricsWriteTimeout))
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := httpserver.Run(ctx, srv, ln, nil); err != nil {
			slog.Error("metrics listener stopped", "address", addr, "error", err)
		}
	}()
	slog.Info("metrics listener started", "address", ln.Addr().String())
	return func() {
		cancel()
		<-done
	}, nil
}
