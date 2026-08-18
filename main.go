package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/cplieger/health"
)

// --- Main ---

// main dispatches on the first argument: `health` runs the Docker probe,
// `run` triggers one Renovate pass via the daemon's socket and exits with
// that run's result (the external-trigger entry point; any further arguments
// are passed through to Renovate as repository slugs), and `daemon` — the
// default when no argument is given — runs the long-lived daemon that owns
// all runs. Any other subcommand is rejected loudly with exit code 2.
func main() {
	// CLI health probe for the Docker healthcheck. RunProbe calls os.Exit, so
	// this is checked before the subcommand switch; the logger is configured
	// first so config-parse warnings from probeOptions (an unparseable
	// SCHED_INTERVAL / SCHED_TIMEOUT) come out as logfmt like every other line.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		setupLogger()
		health.RunProbe(healthMarkerPath, probeOptions()...)
	}

	cmd := "daemon"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "daemon":
		if err := runDaemon(context.Background(), socketPath, defaultCommandRunner); err != nil {
			os.Exit(1)
		}
	case "run":
		os.Exit(runClient(socketPath, os.Args[2:]))
	default:
		setupLogger()
		slog.Error("unknown subcommand", "command", cmd, "valid", "daemon, run, health")
		os.Exit(2)
	}
}
