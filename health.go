package main

import "github.com/cplieger/health"

// healthMarkerPath is where the health marker file lives. Docker's
// HEALTHCHECK re-invokes the binary with the `health` subcommand, which
// stats this path. /tmp is writable by Renovate's non-root user (and is a
// tmpfs in read-only deployments), the same place an external `run` writes
// the marker so the long-running container's probe reflects it.
const healthMarkerPath = health.DefaultPath
