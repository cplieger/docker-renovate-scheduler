package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/cplieger/health"
)

func main() {
	setupLogger()

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
		slog.Error("unknown subcommand", "command", cmd, "valid", "daemon, run, health")
		os.Exit(2)
	}
}
