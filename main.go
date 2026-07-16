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
// are passed through to Renovate as repository slugs), and anything else
// (including no argument) runs the long-lived daemon that owns all runs.
func main() {
	// CLI health probe for the Docker healthcheck. Checked before the logger
	// is configured because RunProbe calls os.Exit.
	if len(os.Args) > 1 && os.Args[1] == "health" {
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
