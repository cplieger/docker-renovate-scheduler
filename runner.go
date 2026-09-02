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
	args = make([]string, 0, len(repos)+1)
	args = append(args, renovateCmd)
	args = append(args, repos...)
	return renovateEntrypoint, args
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

func withDumbInitInGroup(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	// Clone because env can be a job payload still read by the result path.
	return append(slices.Clone(env), "DUMB_INIT_SETSID=0")
}

type stopRequested func() error

type runOutcome int

const (
	runComplete runOutcome = iota
	runFailed
	runCancelled
	// A surviving group can still write the base directory.
	runContained
)

func runRenovateOnce(ctx context.Context, stopping stopRequested,
	timeout time.Duration, trig string, p runPayload,
	newCmd scheduler.CommandRunner,
) runOutcome {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := renovateInvocation(p.Repos)

	start := time.Now()
	slog.Info("renovate run starting", "trigger", trig, "repos", p.Repos, "timeout", timeout)

	cmd := newCmd(runCtx, name, args...)
	cmd.Env = withDumbInitInGroup(p.Env)
	if startErr := cmd.Start(); startErr != nil {
		slog.Error("renovate run failed",
			"trigger", trig, "duration_ms", time.Since(start).Milliseconds(), "error", startErr)
		return runFailed
	}
	if stopping() != nil {
		stopUncommittedRun(cmd)
		slog.Warn("renovate run cancelled by shutdown at start", "trigger", trig, "repos", p.Repos)
		return runCancelled
	}
	runErr := cmd.Wait()
	durationMs := time.Since(start).Milliseconds()
	// Read the deadline before the containment sweep consumes more time.
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	survived := sweepRunGroupOrWarn(cmd, trig)

	switch {
	case runErr == nil:
		if survived {
			return runContained
		}
		slog.Info("renovate run complete", "trigger", trig, "duration_ms", durationMs)
		return runComplete
	case timedOut:
		slog.Error("renovate run timed out",
			"trigger", trig, "duration_ms", durationMs, "timeout", timeout)
	default:
		logRunFailure(trig, durationMs, runErr)
	}
	if survived {
		return runContained
	}
	return runFailed
}

// abortExitCode is what a shell reports when its child dies on SIGABRT.
// Renovate's entrypoint is a shell, so node's fatal errors reach the daemon as
// this exit code rather than as a signal on the daemon's own child.
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
	if !errors.As(runErr, &exitErr) {
		return runDiagnosis{}, false
	}
	if !abortedOnSIGABRT(exitErr) {
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

// abortedOnSIGABRT reports whether the run died on SIGABRT, either signalled
// directly or reported by an intervening shell as 128+SIGABRT.
func abortedOnSIGABRT(exitErr *exec.ExitError) bool {
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal() == syscall.SIGABRT
	}
	return exitErr.ExitCode() == abortExitCode
}

func sweepRunGroupOrWarn(cmd *exec.Cmd, trig string) (survived bool) {
	if !sweepRunProcessGroup(cmd) {
		slog.Warn("renovate run process group survived the kill sweep; halting run admission to prevent an overlapping run",
			"trigger", trig, "pid", cmd.Process.Pid)
		return true
	}
	return false
}

const runGroupPollInterval = 50 * time.Millisecond

func stopUncommittedRun(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { defer close(done); _ = cmd.Wait() }()

	if waitForRunProcessGroup(cmd, done, scheduler.DefaultGrace) {
		return
	}
	if !sweepRunProcessGroup(cmd) {
		slog.Warn("uncommitted run process group survived the grace-expiry kill sweep; shutdown may leave it running",
			"pid", cmd.Process.Pid)
	}
	<-done
}

func waitForRunProcessGroup(cmd *exec.Cmd, waitDone <-chan struct{}, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(runGroupPollInterval)
	defer poll.Stop()

	reaped := false
	for {
		if reaped && runProcessGroupGone(cmd) {
			return true
		}
		select {
		case <-waitDone:
			reaped = true
			waitDone = nil
		case <-poll.C:
		case <-deadline.C:
			return false
		}
	}
}

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
