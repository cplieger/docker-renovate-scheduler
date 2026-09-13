package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"github.com/cplieger/scheduler/v4"
)

const (
	// The image entrypoint initializes containerbase for docker-exec runs.
	renovateEntrypoint = "/usr/local/sbin/renovate-entrypoint.sh"
	renovateCmd        = "renovate"
)

func renovateInvocation(repos []string) (name string, args []string) {
	return renovateEntrypoint, append([]string{renovateCmd}, repos...)
}

var defaultCommandRunner scheduler.CommandRunner = func() scheduler.CommandRunner {
	base := scheduler.NewCommandRunner(scheduler.DefaultGrace)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := base(ctx, name, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			// Signal the group so package-manager children stop with Renovate.
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return cmd
	}
}()

// withChildOverrides appends the scheduler's fixed child-environment overrides
// after env, where os/exec's last-duplicate-wins dedup lets them beat any forwarded value.
func withChildOverrides(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	// Concat copies: env is the caller's slice, so a bare append could write
	// into its spare capacity.
	return slices.Concat(env, []string{
		// Keeps the per-run dumb-init from detaching Renovate into a new
		// session outside the Setpgid group the kill sweep addresses.
		"DUMB_INIT_SETSID=0",
	})
}

type runOutcome int

const (
	runComplete runOutcome = iota
	// Renovate's own output carries the cause of a non-zero exit.
	runFailed
	runStartFailed
	runTimedOut
	// A surviving group can still write the base directory.
	runContained
)

func runRenovateOnce(ctx context.Context, timeout time.Duration, trig string, p runPayload, newCmd scheduler.CommandRunner) runOutcome {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := renovateInvocation(p.Repos)

	start := time.Now()
	slog.Info("renovate run starting", "trigger", trig, "repos", p.Repos, "timeout", timeout)

	cmd := newCmd(runCtx, name, args...)
	cmd.Env = withChildOverrides(p.Env)
	if startErr := cmd.Start(); startErr != nil {
		logRunFailure(trig, time.Since(start).Milliseconds(), startErr)
		return runStartFailed
	}
	runErr := cmd.Wait()
	durationMs := time.Since(start).Milliseconds()
	// Read the deadline before the containment sweep consumes more time.
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	survived := !sweepRunProcessGroup(cmd)

	failure := runFailed
	switch {
	case runErr == nil:
		if survived {
			return runContained
		}
		slog.Info("renovate run complete", "trigger", trig, "duration_ms", durationMs)
		return runComplete
	case timedOut:
		failure = runTimedOut
		slog.Error("renovate run timed out",
			"trigger", trig, "duration_ms", durationMs, "timeout", timeout)
	default:
		logRunFailure(trig, durationMs, runErr)
	}
	if survived {
		return runContained
	}
	return failure
}

// abortExitCode is how node's SIGABRT reaches the daemon: the entrypoint chain
// execs into dumb-init, which reports a signal-killed child as 128+signal.
const abortExitCode = 128 + int(syscall.SIGABRT)

// runDiagnosis carries a named cause for a run failure and its remedy.
type runDiagnosis struct {
	cause string
	fix   string
}

// logRunFailure records a failed run, naming a likely cause when the exit
// status identifies one so the operator reads a remedy instead of a number.
// The message string is fixed because the README's alerting rules key on it.
func logRunFailure(trig string, durationMs int64, runErr error) {
	if diag, ok := abortDiagnosis(runErr); ok {
		slog.Error("renovate run failed",
			"trigger", trig, "duration_ms", durationMs, "error", runErr,
			"likely_cause", diag.cause, "fix", diag.fix)
		return
	}
	slog.Error("renovate run failed",
		"trigger", trig, "duration_ms", durationMs, "error", runErr)
}

// abortDiagnosis names the likely cause of a SIGABRT-shaped run failure. Node
// aborts on a V8 fatal error, and for Renovate that is dominated by heap
// exhaustion. It needs saying because the bare status points away from the
// cause: node sizes its heap from the container memory limit and dies at
// roughly half of it, so the kernel OOM killer never fires and the cgroup's
// own counters stay at zero.
func abortDiagnosis(runErr error) (runDiagnosis, bool) {
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != abortExitCode {
		return runDiagnosis{}, false
	}
	return runDiagnosis{
		cause: "renovate aborted; for node this is usually a JavaScript heap exhaustion",
		fix: "node sizes its heap from the container memory limit, so raise mem_limit; " +
			"on a resident deployment also set RENOVATE_X_SQLITE_PACKAGE_CACHE=true, because " +
			"the file package cache's end-of-run collection grows with the cache. " +
			"See the README, 'Memory and the package cache'",
	}, true
}

const runGroupPollInterval = 50 * time.Millisecond

func runProcessGroupGone(cmd *exec.Cmd) bool {
	return errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
}

func sweepRunProcessGroup(cmd *exec.Cmd) bool {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	deadline := time.NewTimer(scheduler.DefaultGrace)
	defer deadline.Stop()
	poll := time.NewTicker(runGroupPollInterval)
	defer poll.Stop()
	for {
		if runProcessGroupGone(cmd) {
			return true
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			return runProcessGroupGone(cmd)
		}
	}
}
