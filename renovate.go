package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// --- Renovate engine ---

const (
	// renovateEntrypoint is the renovate image's own entrypoint chain
	// (renovate-entrypoint.sh, which exec-chains to the containerbase
	// docker-entrypoint.sh). It establishes the containerbase environment —
	// CONTAINERBASE / CONTAINERBASE_ENV, USER_* , locale, and the
	// ~/.local/bin PATH additions — that Renovate's default
	// binarySource=install needs to install package-manager toolchains (go,
	// npm, etc.) at runtime for lockfile maintenance.
	//
	// Every Renovate invocation is routed through this entrypoint so it gets
	// that environment whether the scheduler was started by the image
	// ENTRYPOINT (daemon mode) or by a bare `docker exec` (the external `run`
	// trigger) — a bare exec does NOT inherit the entrypoint's environment.
	renovateEntrypoint = "/usr/local/sbin/renovate-entrypoint.sh"

	// renovateCmd is the Renovate CLI, resolved on PATH by the entrypoint.
	renovateCmd = "renovate"
)

// renovateInvocation returns the command name and argument slice for one
// Renovate pass, routed through the image entrypoint (see renovateEntrypoint).
// repoArgs, when non-empty, are positional repository slugs passed straight
// to Renovate to restrict the run; empty means Renovate's own repositories /
// autodiscover configuration decides the set.
func renovateInvocation(repoArgs []string) (name string, args []string) {
	args = make([]string, 0, len(repoArgs)+1)
	args = append(args, renovateCmd)
	args = append(args, repoArgs...)
	return renovateEntrypoint, args
}

// commandRunner constructs a configured *exec.Cmd. It decouples
// orchestration from subprocess construction so tests can inject a fake.
type commandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd

// defaultCommandRunner returns an exec.Cmd that streams the child's
// stdout/stderr straight through to the container's stdout/stderr and shuts
// down gracefully (SIGTERM on context cancellation, 5s grace before
// SIGKILL). Renovate is the logging authority — set LOG_FORMAT=json and let
// Alloy/Loki ingest its structured output; the scheduler neither captures
// nor parses it. The subprocess inherits the parent environment, so
// Renovate's RENOVATE_* configuration and tokens flow through unchanged.
func defaultCommandRunner(ctx context.Context, name string, cmdArgs ...string) *exec.Cmd {
	// #nosec G702 -- fixed binary path (renovateInvocation returns the const
	// renovateEntrypoint) and a discrete argv slice exec'd with no shell; the
	// only variable args are repo slugs forwarded to Renovate, supplied by the
	// operator via the scheduler / docker exec, not an untrusted boundary.
	cmd := exec.CommandContext(ctx, name, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// runRenovatePass acquires the overlap lock and runs one Renovate pass. It
// returns ok=true on success — either Renovate exited 0, or the lock was
// already held so this pass is a deliberate no-op — and ok=false when
// Renovate exited non-zero (or the lock could not be acquired due to an
// error).
//
// The whole pass is guarded by an advisory file lock so the built-in ticker
// (in-process) and an external `run` exec (cross-process) never run two
// Renovate processes against the same base dir at once.
func runRenovatePass(ctx context.Context, timeout time.Duration, trigger string, repoArgs []string, newCmd commandRunner) (ok bool) {
	lock, locked, lockErr := tryLock(lockFilePath)
	if lockErr != nil {
		slog.Error("cannot acquire run lock",
			"trigger", trigger, "path", lockFilePath, "error", lockErr)
		return false
	}
	if !locked {
		slog.Info("renovate already running, skipping overlapping request", "trigger", trigger)
		return true
	}
	defer lock.unlock()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := renovateInvocation(repoArgs)

	start := time.Now()
	slog.Info("renovate run starting", "trigger", trigger, "repos", repoArgs, "timeout", timeout)

	runErr := newCmd(runCtx, name, args...).Run()
	durationMs := time.Since(start).Milliseconds()

	if runErr != nil {
		slog.Error("renovate run failed",
			"trigger", trigger, "duration_ms", durationMs, "error", runErr)
		return false
	}
	slog.Info("renovate run complete", "trigger", trigger, "duration_ms", durationMs)
	return true
}
