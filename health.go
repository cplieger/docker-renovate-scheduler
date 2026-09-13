package main

import "github.com/cplieger/health"

const healthMarkerPath = health.DefaultPath

func probeOptions() []health.ProbeOption {
	interval, scheduleEnabled := loadInterval()
	if !scheduleEnabled {
		return nil
	}
	timeout := loadRunTimeout()
	lease := health.Lease{Interval: interval, Cycles: 2, Timeout: timeout, Attempts: 1}
	return []health.ProbeOption{health.WithMaxAge(lease.Duration())}
}
