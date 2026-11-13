package main

import (
	"math"
	"net/http"
	"time"

	"github.com/cplieger/go/health/v2"
)

// healthMarkerPath is the file the daemon mirrors its verdict to; Docker's
// HEALTHCHECK re-invokes the binary with the `health` subcommand, which reads it.
var healthMarkerPath = health.MarkerPath("docker-renovate-scheduler")

// probeLease is the lease the health subcommand arms: the daemon's runLease
// for the settings the environment holds.
func probeLease() time.Duration {
	interval, scheduleEnabled := loadInterval()
	return runLease(interval, scheduleEnabled, loadRunTimeout(), appConfigured())
}

// runLease is how old a healthy verdict may grow before the schedule counts as
// stuck: two intervals plus one run's budget, RUN_TIMEOUT, and in GitHub App
// mode the token request's tokenMintBudget before it. External mode arms none
// (0).
func runLease(interval time.Duration, scheduleEnabled bool, runTimeout time.Duration, appMode bool) time.Duration {
	if !scheduleEnabled {
		return 0
	}
	run := runTimeout
	if appMode {
		run = min(runTimeout, math.MaxInt64-tokenMintBudget) + tokenMintBudget
	}
	return health.Lease{Interval: interval, Cycles: 2, Timeout: run, Attempts: 1}.Duration()
}

// Reasons /healthz lists. Fixed sentences, because the route answers with no login.
const (
	reasonRunFailed = "the last run failed, no run has finished since a start with a run due, or the container is stopping"
	reasonStuck     = "no run has finished within two run intervals plus one run's time budget, so the schedule may be stuck"
)

// newHealthz serves st's verdict as one check per fault, so each names itself.
func newHealthz(st *health.Status, lease time.Duration) http.Handler {
	return health.CheckHandler(
		health.Check{Signal: st, Reason: reasonRunFailed},
		health.Check{Signal: st.Fresh(lease), Reason: reasonStuck},
	)
}
