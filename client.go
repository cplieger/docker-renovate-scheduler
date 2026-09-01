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

func runClient(socketPath string, repos []string) int {
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

func finishResult(ev trigger.Event, repos []string) int {
	if ev.OK {
		slog.Info("triggered run complete", "repos", repos, "duration_ms", ev.DurationMs)
		return 0
	}
	reason := cmp.Or(ev.Reason, "renovate exited non-zero (see the container log stream)")
	slog.Error("triggered run failed", "repos", repos, "duration_ms", ev.DurationMs, "reason", reason)
	return 1
}
