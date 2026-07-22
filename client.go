package main

import (
	"errors"
	"log/slog"
	"os"

	"github.com/cplieger/scheduler/v2/trigger"
)

// --- `run` subcommand: the trigger client ---
//
// A thin adapter over the scheduler library's synchronous trigger client: it
// forwards the request — positional repo slugs plus this process's complete
// environment — and blocks until the daemon reports the run's result, exiting
// 0/1 with that result. The external trigger contract is unchanged
// (`docker exec renovate docker-renovate-scheduler run [repo …]`, exit code =
// run outcome, `-e RENOVATE_X=…` overrides reach Renovate), and the run
// itself executes inside the daemon: its logs land on the container's stdout
// stream in every mode, while this process's output is only its own lifecycle
// lines. The library owns the transport (dial, wire order, failure taxonomy);
// this file owns the wording — the lifecycle lines an Ofelia job log or a
// Komodo action captures.

// runClient performs one triggered run via the daemon at socketPath and
// returns the process exit code: 0 on success, 1 on failure (including a
// rejected or cancelled request, or a daemon that cannot be reached).
func runClient(socketPath string, repos []string) int {
	setupLogger()

	final, err := trigger.Submit(socketPath, runPayload{Repos: repos, Env: os.Environ()}, func(ev trigger.Event) {
		switch ev.Kind {
		case trigger.EventQueued:
			slog.Info("triggered run accepted", "repos", repos)
		case trigger.EventStarted:
			slog.Info("triggered run started", "repos", repos,
				"logs", "full Renovate output is on the container log stream")
		}
	})
	switch {
	case errors.Is(err, trigger.ErrUnreachable):
		slog.Error("cannot reach the scheduler daemon",
			"path", socketPath, "error", err,
			"hint", "the daemon (PID 1) owns all runs; check the container is up and this exec runs as the container's user (the socket is owner-only)")
		return 1
	case errors.Is(err, trigger.ErrSend):
		slog.Error("cannot send run request", "error", err)
		return 1
	case err != nil:
		slog.Error("connection lost before the run completed (daemon stopped?)", "error", err)
		return 1
	}
	return finishResult(final, repos)
}

// finishResult logs the final outcome and maps it to the exit code.
func finishResult(ev trigger.Event, repos []string) int {
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
