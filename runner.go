package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
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

// childDefaults are Renovate settings the scheduler supplies only when env
// carries no entry for the key, so an explicit value, even an empty one, wins.
var childDefaults = []string{
	// Renovate exits 0 when a repository ends in a host or platform error, so a
	// run whose every clone was refused would read as complete and keep health
	// green (#1336). With this, such a run exits with a code naming the error,
	// which the executor turns into a failure once it lasts (failureGrace).
	"RENOVATE_EXIT_CODE_FOR_ERRORS=true",
}

// withChildOverrides appends the missing childDefaults, then the scheduler's
// fixed child-environment overrides, after env, where os/exec's
// last-duplicate-wins dedup lets the overrides beat any forwarded value.
func withChildOverrides(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	var defaults []string
	for _, kv := range childDefaults {
		key, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, key+"=") }) {
			defaults = append(defaults, kv)
		}
	}
	// Concat copies: env is the caller's slice, so a bare append could write
	// into its spare capacity.
	return slices.Concat(env, defaults, []string{
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
	// A repository ended in an error state (Renovate's exit 3 to 8) and Renovate
	// went on with the others; the executor decides whether the run fails.
	runRepositoryError
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
		if logRunFailure(trig, durationMs, runErr) {
			failure = runRepositoryError
		}
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
// status identifies one so the operator reads a remedy instead of a number,
// and reports whether it was a repository error. A repository error is a
// warning here, because a platform outage causes most of them; the executor
// escalates one that lasts. The message strings are fixed because the shipped
// alert rules key on them.
func logRunFailure(trig string, durationMs int64, runErr error) (repositoryError bool) {
	if diag, ok := repositoryErrorDiagnosis(runErr); ok {
		slog.Warn("renovate run ended in a repository error",
			"trigger", trig, "duration_ms", durationMs, "error", runErr,
			"likely_cause", diag.cause, "fix", diag.fix)
		return true
	}
	if diag, ok := abortDiagnosis(runErr); ok {
		slog.Error("renovate run failed",
			"trigger", trig, "duration_ms", durationMs, "error", runErr,
			"likely_cause", diag.cause, "fix", diag.fix)
		return false
	}
	slog.Error("renovate run failed",
		"trigger", trig, "duration_ms", durationMs, "error", runErr)
	return false
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
			"See docs/configuration.md, 'Memory and the package cache'",
	}, true
}

// repositoryErrorCauses are the exit codes Renovate's exitCodeForErrors gives a
// run whose repository ended in an error state (childDefaults turns it on).
var repositoryErrorCauses = map[int]string{
	3: "a repository ended in a Renovate system error, such as a full disk",
	4: "a repository ended in a platform error, such as a rate limit or credentials the platform rejected",
	5: "a repository ended in a configuration error",
	6: "a repository ended in a temporary error",
	7: "a repository ended in an external host error, a lockfile error or missing credentials, " +
		"such as a platform outage or a clone or push the platform refused",
	8: "a repository ended in an unknown error",
}

func repositoryErrorDiagnosis(runErr error) (runDiagnosis, bool) {
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return runDiagnosis{}, false
	}
	cause, ok := repositoryErrorCauses[exitErr.ExitCode()]
	if !ok {
		return runDiagnosis{}, false
	}
	return runDiagnosis{
		cause: cause,
		fix: "Renovate's 'Repository finished' line names the repository and its result; " +
			"Renovate goes on with the other repositories, so their updates still land. " +
			"A run fails only once such errors have lasted 2h; see docs/monitoring.md, 'Repository errors'",
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
