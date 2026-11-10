package main

import (
	"math"
	"net/http"
	"time"

	"github.com/cplieger/go/health"
)

const healthMarkerPath = health.DefaultPath

func probeOptions() []health.ProbeOption {
	interval, scheduleEnabled := loadInterval()
	lease := runLease(interval, scheduleEnabled, loadRunTimeout(), appConfigured())
	if lease <= 0 {
		return nil
	}
	return []health.ProbeOption{health.WithMaxAge(lease)}
}

// runLease is how old the marker may grow before the schedule counts as
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

// newHealthz serves the verdict the health subcommand reads from marker while
// the marker directory is writable, as one check per fault so each names itself.
func newHealthz(marker *health.Marker, lease time.Duration) http.Handler {
	return health.CheckHandler(
		health.Check{Signal: marker, Reason: reasonRunFailed},
		health.Check{Signal: notStale{marker: marker, lease: lease}, Reason: reasonStuck},
	)
}

// notStale fails a marker that reads healthy but is past the lease, so an
// absent marker reports reasonRunFailed alone and not reasonStuck too.
type notStale struct {
	marker *health.Marker
	lease  time.Duration
}

func (s notStale) Healthy() bool {
	return s.marker.Within(s.lease).Healthy() || !s.marker.Healthy()
}
