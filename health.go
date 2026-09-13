package main

import "github.com/cplieger/health"

const healthMarkerPath = health.DefaultPath

func probeOptions() []health.ProbeOption {
	interval, scheduleEnabled := loadInterval()
	if !scheduleEnabled {
		return nil
	}
	timeout := loadRunTimeout()
	return []health.ProbeOption{health.WithMaxAge(2*interval + timeout)}
}
