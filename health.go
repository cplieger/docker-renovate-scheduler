package main

import (
	"time"

	"github.com/cplieger/health"
)

const healthMarkerPath = health.DefaultPath

func probeOptions() []health.ProbeOption {
	interval, scheduleEnabled := loadInterval()
	if !scheduleEnabled {
		return nil
	}
	timeout := loadRunTimeout()
	// Saturate because a non-positive max age disables freshness checks.
	const maxDuration = time.Duration(1<<63 - 1)
	maxAge := maxDuration
	if interval <= (maxDuration-timeout)/2 {
		maxAge = 2*interval + timeout
	}
	return []health.ProbeOption{health.WithMaxAge(maxAge)}
}
