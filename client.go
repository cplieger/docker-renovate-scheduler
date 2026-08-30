package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cplieger/scheduler/v4/trigger"
)

// --- `run` subcommand: the trigger client ---
//
// The library owns the transport (dial, wire order, failure taxonomy);
// this file owns the wording — the lifecycle lines the external
// trigger's own log captures (a job scheduler's log, a deployment
// action's output).

// runClient performs one triggered run via the daemon at socketPath and
// returns the process exit code: 0 on success, 1 on failure (including a
// rejected or cancelled request, or a daemon that cannot be reached).
func runClient(socketPath string, repos []string) int {
	// The daemon owns the run; this process only waits for its result, and
	// that wait is unbounded by contract. Bind it to the terminal so an
	// operator interrupting the `docker exec` unwinds here -- closing the
	// connection, which the daemon observes -- instead of leaving the socket
	// half-open until the kernel reaps this process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	accepted := false
	final, err := trigger.Submit(ctx, socketPath, runPayload{Repos: repos, Env: os.Environ()}, func(ev trigger.Event) {
		switch ev.Kind {
		case trigger.EventQueued:
			accepted = true
			slog.Info("triggered run accepted", "repos", repos)
		case trigger.EventStarted:
			slog.Info("triggered run started", "repos", repos,
				"logs", "full Renovate output is on the container log stream")
		}
	})
	switch {
	case err != nil && ctx.Err() != nil:
		// The operator interrupted, so the exit code reports "outcome
		// unknown to me", not "the run failed". Before EventQueued the
		// client has not observed acceptance and cannot claim the run
		// continues. This arm is first because a cancelled dial also
		// satisfies ErrUnreachable, and a cancelled send carries no context
		// error at all, so no sentinel test catches every interrupt window.
		if accepted {
			slog.Warn("interrupted while waiting for the run; the run the daemon accepted continues there")
		} else {
			slog.Warn("interrupted while waiting for the run; acceptance was not observed, so the scheduler may or may not have accepted the request")
		}
		return 1
	case errors.Is(err, trigger.ErrUnreachable):
		slog.Error("cannot reach the scheduler daemon",
			"path", socketPath, "error", err,
			"hint", "the daemon owns all runs; check the container is up and this exec runs as the container's user (the socket is owner-only)")
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
	reason := cmp.Or(ev.Reason, "renovate exited non-zero (see the container log stream)")
	slog.Error("triggered run failed", "repos", repos, "duration_ms", ev.DurationMs, "reason", reason)
	return 1
}
