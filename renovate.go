package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/cplieger/scheduler"
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

// defaultCommandRunner builds one Renovate subprocess command: the shared
// scheduler library supplies graceful shutdown (SIGTERM on context
// cancellation, then a DefaultGrace 5s window before SIGKILL), and this
// wrapper streams the child's stdout/stderr straight through to the
// container's stdout/stderr. Renovate is the logging authority — set
// LOG_FORMAT=json and let Alloy/Loki ingest its structured output; the
// scheduler neither captures nor parses it. The subprocess inherits the parent
// environment, so Renovate's RENOVATE_* configuration and tokens flow through
// unchanged. The binary path is the fixed renovateEntrypoint const and the
// only variable args are operator-supplied repo slugs, so there is no
// untrusted-input boundary here.
var defaultCommandRunner scheduler.CommandRunner = func() scheduler.CommandRunner {
	base := scheduler.NewCommandRunner(scheduler.DefaultGrace)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := base(ctx, name, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}
}()

// rerunFlag is the shared scheduler library's single-slot coalescing flag
// backing the overlap guard's "max 1 wait" rerun: a trigger arriving while a
// run holds the lock sets it, and the active holder — which clears it before
// each pass — reruns once on completion if it was set during the pass.
var rerunFlag = scheduler.NewRerunFlag(rerunFlagPath)

// maxCoalescedReruns bounds how many times one holder will rerun to drain
// queued triggers before releasing the lock, so a relentless trigger source
// can't pin it indefinitely. A realistic burst (many release webhooks at
// once) collapses into one or two reruns because the flag is single-slot; the
// cap only bites under a pathological continuous storm, after which the holder
// releases the lock and the next trigger (or the next scheduled tick) resumes.
const maxCoalescedReruns = 8

// runRenovatePass acquires the overlap lock and runs Renovate, coalescing
// overlapping triggers. It returns ok=true on success — either the final pass
// exited 0, or the lock was already held so this call only queued a rerun (a
// deliberate no-op) — and ok=false when a pass exited non-zero (or the lock
// could not be acquired due to an error).
//
// The pass is guarded by an advisory file lock so the built-in ticker
// (in-process) and an external `run` exec (cross-process) never run two
// Renovate processes against the same base dir at once. Rather than dropping a
// trigger that arrives while a run is in flight, the loser sets a single-slot
// rerun flag; the holder clears it before each pass and, if a trigger set it
// during the pass, reruns on completion (bounded by maxCoalescedReruns). The
// flag is cleared before the pass, not after, so a trigger arriving mid-run is
// never lost.
// shutdownCtx governs whether to START a pass/rerun (shutdown-aware); runCtx
// governs execution lifetime (decoupled from shutdown in built-in mode, equal
// to shutdownCtx in external mode). Splitting them lets an in-flight pass drain
// uncancelled while a queued rerun is still suppressed once shutdown is
// signalled, so a stop request is never followed by a fresh run.
func runRenovatePass(shutdownCtx, runCtx context.Context, timeout time.Duration, trigger string, repoArgs []string, newCmd scheduler.CommandRunner) (ok bool) {
	lock, locked, lockErr := scheduler.TryLock(lockFilePath)
	if !locked && lockErr == nil {
		// A run is already in flight. Queue a single coalesced rerun instead
		// of dropping this trigger; the active holder reruns on completion.
		rerunFlag.Set()
		// Narrow the lost-wakeup window: the holder may have already passed its
		// final rerunFlag.Pending() check and be between there and releasing the
		// lock when we set the flag above, leaving no holder to observe it.
		// Retry the lock once; if the holder has since released it we now own the
		// run and drain the flag ourselves. A residual window remains (holder still
		// holding across both our attempts); it loses no work -- the next
		// tick/trigger clears the flag and runs -- only latency until that run.
		lock, locked, lockErr = scheduler.TryLock(lockFilePath)
	}
	if lockErr != nil {
		slog.Error("cannot acquire run lock",
			"trigger", trigger, "path", lockFilePath, "error", lockErr)
		return false
	}
	if !locked {
		slog.Info("renovate already running, queued rerun", "trigger", trigger)
		return true
	}
	defer lock.Unlock()

	for reruns := 0; ; reruns++ {
		// Clear before the pass so only triggers arriving *during* it queue
		// the next rerun (no lost wakeups). The first iteration also clears
		// any stale flag left by a holder that exited mid-window.
		rerunFlag.Clear()

		passTrigger := trigger
		if reruns > 0 {
			passTrigger = trigger + "+rerun"
		}
		if ok = runRenovateOnce(runCtx, timeout, passTrigger, repoArgs, newCmd); !ok {
			// A failed or timed-out pass stops the loop: don't hammer a failing
			// Renovate. Any queued trigger is left for the next scheduled run
			// or external trigger to retry.
			return false
		}
		if !rerunFlag.Pending() {
			return true
		}
		// Shutdown gate: a rerun is queued, but if shutdown has been signalled
		// don't START a fresh pass. In built-in mode runCtx is decoupled from
		// shutdown (context.WithoutCancel), so without this check the loop would
		// launch another runRenovateOnce that drains for a full SCHED_TIMEOUT
		// after SIGTERM -- risking the stop_grace_period overrun (exit-137
		// SIGKILL) this drain feature exists to prevent. The in-flight pass
		// already completed above; we only defer the *next* one.
		if shutdownCtx.Err() != nil {
			slog.Info("shutdown signalled during run; deferring queued rerun", "trigger", trigger)
			return true
		}
		if reruns >= maxCoalescedReruns {
			slog.Warn("coalesced rerun cap reached; deferring remaining work to next trigger",
				"trigger", trigger, "cap", maxCoalescedReruns)
			return true
		}
		slog.Info("trigger arrived during run; coalescing rerun", "trigger", trigger, "rerun", reruns+1)
	}
}

// runRenovateOnce executes exactly one Renovate pass under an already-held
// overlap lock and reports whether it exited cleanly. The pass is bounded by
// timeout (SCHED_TIMEOUT); on cancellation the command runner sends SIGTERM
// with a short grace before SIGKILL.
func runRenovateOnce(ctx context.Context, timeout time.Duration, trigger string, repoArgs []string, newCmd scheduler.CommandRunner) (ok bool) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := renovateInvocation(repoArgs)

	start := time.Now()
	slog.Info("renovate run starting", "trigger", trigger, "repos", repoArgs, "timeout", timeout)

	runErr := newCmd(runCtx, name, args...).Run()
	durationMs := time.Since(start).Milliseconds()

	switch {
	case runErr == nil:
		slog.Info("renovate run complete", "trigger", trigger, "duration_ms", durationMs)
		return true
	case ctx.Err() != nil:
		// Parent context cancelled: the external `run` process is shutting
		// down, not a Renovate failure. Warn (not Error) so a clean shutdown
		// doesn't trip the level=error alert. In built-in mode the run ctx is
		// decoupled from the shutdown signal (see runBuiltin), so this branch
		// fires only for the cancelable external `run` path.
		slog.Warn("renovate run interrupted by shutdown",
			"trigger", trigger, "duration_ms", durationMs, "cause", context.Cause(ctx))
		return false
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// The run exceeded SCHED_TIMEOUT. Logged distinctly from a genuine
		// non-zero Renovate exit so operators can tell a slow run from a
		// real failure during triage.
		slog.Error("renovate run timed out",
			"trigger", trigger, "duration_ms", durationMs, "timeout", timeout)
		return false
	default:
		slog.Error("renovate run failed",
			"trigger", trigger, "duration_ms", durationMs, "error", runErr)
		return false
	}
}
