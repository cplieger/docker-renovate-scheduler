package main

import "github.com/cplieger/health"

// healthMarkerPath is where the health marker file lives. Docker's
// HEALTHCHECK re-invokes the binary with the `health` subcommand, which
// stats this path. /tmp is writable by Renovate's non-root user (and is a
// tmpfs in read-only deployments). The daemon — the single owner of every
// run — is the marker's single writer.
const healthMarkerPath = health.DefaultPath

// probeOptions returns the healthcheck probe's freshness policy. Built-in
// mode arms a max-age deadline: the executor refreshes the marker after every
// run, so a marker present but never refreshed means the interval loop is
// wedged and the container should probe unhealthy and restart. Two intervals
// plus one run timeout is generous headroom for a slow-but-progressing loop.
// External mode keeps no deadline: an idle container between sparse triggers
// is healthy, and a trigger-written marker must not expire.
func probeOptions() []health.ProbeOption {
	interval, scheduleEnabled := loadInterval()
	if !scheduleEnabled {
		return nil
	}
	return []health.ProbeOption{health.WithMaxAge(2*interval + loadRunTimeout())}
}
