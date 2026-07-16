package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"time"
)

// --- `run` subcommand: the trigger client ---
//
// A thin synchronous client for the daemon's trigger socket: it forwards the
// request — positional repo slugs plus this process's complete environment —
// and blocks until the daemon reports the run's result, exiting 0/1 with that
// result. The external trigger contract is unchanged from the previous
// design (`docker exec renovate docker-renovate-scheduler run [repo …]`,
// exit code = run outcome, `-e RENOVATE_X=…` overrides reach Renovate), but
// the run itself now executes inside the daemon: its logs land on the
// container's stdout stream in every mode, and this process's output is only
// its own lifecycle lines.

// dialTimeout bounds the connection attempt: the daemon is PID 1 in the same
// container, so anything slower than instant means it is not accepting.
const dialTimeout = 5 * time.Second

// runClient performs one triggered run via the daemon at socketPath and
// returns the process exit code: 0 on success, 1 on failure (including a
// rejected or cancelled request, or a daemon that cannot be reached).
func runClient(socketPath string, repos []string) int {
	setupLogger()

	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(context.Background(), "unix", socketPath)
	if err != nil {
		slog.Error("cannot reach the scheduler daemon",
			"path", socketPath, "error", err,
			"hint", "the daemon (PID 1) owns all runs; check the container is up and this exec runs as the container's user (the socket is owner-only)")
		return 1
	}
	defer func() { _ = conn.Close() }()

	if err := json.NewEncoder(conn).Encode(wireRequest{Repos: repos, Env: os.Environ()}); err != nil {
		slog.Error("cannot send run request", "error", err)
		return 1
	}
	return awaitResult(conn, repos)
}

// awaitResult consumes the daemon's event stream until the final result.
// Lifecycle is logged to THIS process's stderr — the trigger's own log (an
// Ofelia job log, a webhook action's output) — while the run's full Renovate
// output lands in the container log stream.
func awaitResult(conn io.Reader, repos []string) int {
	dec := json.NewDecoder(conn)
	for {
		var ev wireEvent
		if err := dec.Decode(&ev); err != nil {
			slog.Error("connection lost before the run completed (daemon stopped?)", "error", err)
			return 1
		}
		switch ev.Event {
		case eventQueued:
			slog.Info("triggered run accepted", "repos", repos)
		case eventStarted:
			slog.Info("triggered run started", "repos", repos,
				"logs", "full Renovate output is on the container log stream")
		case eventDone:
			return finishResult(ev, repos)
		default:
			slog.Debug("ignoring unknown event", "event", ev.Event)
		}
	}
}

// finishResult logs the final outcome and maps it to the exit code.
func finishResult(ev wireEvent, repos []string) int {
	if ev.OK {
		slog.Info("triggered run complete", "repos", repos, "duration_ms", ev.DurationMs)
		return 0
	}
	reason := ev.Reason
	if reason == "" {
		reason = "renovate exited non-zero (see the container log stream)"
	}
	slog.Error("triggered run failed", "repos", repos, "duration_ms", ev.DurationMs, "reason", reason)
	return 1
}
