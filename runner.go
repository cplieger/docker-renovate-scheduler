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

// --- Renovate execution ---

const (
	// renovateEntrypoint is the renovate image's own entrypoint chain
	// (renovate-entrypoint.sh, which exec-chains to the containerbase
	// docker-entrypoint.sh), which establishes the containerbase environment
	// Renovate's default binarySource=install needs (the ~/.local/bin PATH
	// additions among them). Every run is routed through it because a run's
	// environment may come from a socket client whose `docker exec` never
	// passed through the image ENTRYPOINT. The Dockerfile asserts this path
	// exists at build time, so a base-image relocation fails the build.
	renovateEntrypoint = "/usr/local/sbin/renovate-entrypoint.sh"

	// renovateCmd is the Renovate CLI, resolved on PATH by the entrypoint.
	renovateCmd = "renovate"
)

// renovateInvocation returns the command name and argument slice for one
// Renovate pass, routed through the image entrypoint (see renovateEntrypoint).
// repos, when non-empty, are positional repository slugs passed straight to
// Renovate to restrict the run; empty means Renovate's own repositories /
// autodiscover configuration decides the set.
func renovateInvocation(repos []string) (name string, args []string) {
	args = make([]string, 0, len(repos)+1)
	args = append(args, renovateCmd)
	args = append(args, repos...)
	return renovateEntrypoint, args
}

// defaultCommandRunner builds one Renovate subprocess command. The scheduler library
// supplies the cancellation escalation (a DefaultGrace 5s window before os/exec's
// SIGKILL); this wrapper's own Cancel sends the SIGTERM, to the child's whole process
// group, and Setpgid creates that group so the init's `docker stop` SIGTERM cannot TERM
// the child out-of-band mid-run (exit 143). Its stdio streams to the daemon's own fds.
// Kept in step with the same wrapper in cplieger/pg-autodump (internal/pg newCommand)
// and the group-half-only copy in cplieger/vibekit; a third consumer is what moves
// this into the scheduler library as an option.
var defaultCommandRunner scheduler.CommandRunner = func() scheduler.CommandRunner {
	base := scheduler.NewCommandRunner(scheduler.DefaultGrace)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := base(ctx, name, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			// Signal the child's whole process group (Setpgid makes it the
			// leader): the run's package-manager grandchildren must stop
			// with it, or they keep writing to the base dir past the timeout.
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return cmd
	}
}()

// withDumbInitInGroup returns the Renovate child's environment — the
// forwarded env when non-nil, else the daemon's own — with DUMB_INIT_SETSID
// forced to 0. The override wins because it is appended LAST and os/exec keeps
// the last value for a duplicate key. The entrypoint chain execs a nested
// per-run `dumb-init --`, and default-mode dumb-init forks Renovate into a NEW
// session/process group below the Setpgid group the scheduler created. Both
// escalation stages address only -cmd.Process.Pid, so a session-escaped tree
// would survive them; =0 keeps dumb-init in signal-proxy mode instead.
func withDumbInitInGroup(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	// slices.Clone, not a bare append: env is the job payload's own slice,
	// still read by the result path, so the child's environment must not be
	// written into the caller's backing array.
	return append(slices.Clone(env), "DUMB_INIT_SETSID=0")
}

// stopRequested reports whether shutdown has been requested, returning the
// cause when it has and nil while admission is still open. It is
// context.Context.Err's shape without the context: a poll, never something a
// consumer can block on.
type stopRequested func() error

// runOutcome is what one Renovate pass amounts to for the executor.
type runOutcome int

const (
	// runComplete: Renovate exited zero and the run's process group is
	// confirmed dead.
	runComplete runOutcome = iota
	// runFailed: the pass did not complete — a start failure, a non-zero
	// exit, or RUN_TIMEOUT. The log record carries which.
	runFailed
	// runCancelled: shutdown won the post-Start handshake, so the child was
	// reaped before the run was ever committed as in-flight.
	runCancelled
	// runContained: the kill sweep could not confirm the process group's
	// death, so a package-manager tree may still be writing the base dir.
	// The caller must NOT start another run against it — this is a
	// containment state, not just a log line.
	runContained
)

// runRenovateOnce executes exactly one Renovate pass and reports its
// runOutcome; runContained is a containment state the caller must not start
// another run against. The pass is bounded by timeout (RUN_TIMEOUT). p.Env,
// when non-nil, replaces the child's environment wholesale (a socket client's
// forwarded environment); nil inherits the daemon's — either way with ONE
// override, DUMB_INIT_SETSID=0 (see withDumbInitInGroup). stopping is polled
// immediately after Start: if shutdown won that race the just-started child
// is reaped (see stopUncommittedRun) and the run reports runCancelled.
func runRenovateOnce(ctx context.Context, stopping stopRequested,
	timeout time.Duration, trig string, p runPayload,
	newCmd scheduler.CommandRunner) runOutcome {
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
	// Snapshot the deadline verdict here, not after the sweep: runCtx's
	// timer keeps running while the sweep confirms the group's death, so a
	// read taken later relabels a run that exited non-zero in time as a
	// timeout.
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	// Wait reaps the leader only, and a zero exit is not proof the group is
	// empty: a hard-crashed Renovate (an OOM-killed node) leaves its
	// package-manager children, and a clean one can leave a backgrounded
	// helper. On an already-empty group the kill is a no-op (ESRCH) and the
	// probe answers on its first pass, so this costs nothing.
	survived := sweepRunGroupOrWarn(cmd, trig)

	switch {
	case runErr == nil:
		if survived {
			// Exited zero, but the group could not be confirmed dead: execute
			// halts admission, so this is not a completed run and must not log
			// the completion line the README's absence alert watches for.
			return runContained
		}
		slog.Info("renovate run complete", "trigger", trig, "duration_ms", durationMs)
		return runComplete
	case timedOut:
		// Logged distinctly from a genuine non-zero Renovate exit so
		// operators can tell a slow run from a real failure during triage.
		slog.Error("renovate run timed out",
			"trigger", trig, "duration_ms", durationMs, "timeout", timeout)
	default:
		// A hard-crashed Renovate (e.g. an OOM-killed node process) exits
		// without reaping its package-manager children.
		slog.Error("renovate run failed",
			"trigger", trig, "duration_ms", durationMs, "error", runErr)
	}
	if survived {
		return runContained
	}
	return runFailed
}

// sweepRunGroupOrWarn force-kills the run's process group via
// sweepRunProcessGroup, warns when the group survives the bounded sweep,
// and reports whether it survived -- the shared post-op bookkeeping for
// every completed run in runRenovateOnce. A true return is the
// fatal containment signal the executor acts on: the group could not be
// confirmed dead, so the base directory must not be handed to another run.
func sweepRunGroupOrWarn(cmd *exec.Cmd, trig string) (survived bool) {
	if !sweepRunProcessGroup(cmd) {
		slog.Warn("renovate run process group survived the kill sweep; halting run admission to prevent an overlapping run",
			"trigger", trig, "pid", cmd.Process.Pid)
		return true
	}
	return false
}

// runGroupPollInterval is how often stopUncommittedRun and
// sweepRunProcessGroup re-probe the child's process group for surviving
// members inside their bounded DefaultGrace windows.
const runGroupPollInterval = 50 * time.Millisecond

// stopUncommittedRun reaps a child that started but lost the post-Start
// shutdown handshake in runRenovateOnce: SIGTERM to the child's process
// group (Setpgid makes it the leader, so package-manager grandchildren stop
// with it), a DefaultGrace window for the WHOLE group to exit cleanly
// (waitForRunProcessGroup), then a group SIGKILL sweep if any member
// lingers. The completion condition is the process group's death, not just
// the direct child's: a leader that honors SIGTERM can exit while a
// package-manager descendant in the same group ignores it, and returning on
// the leader's exit alone would leave that descendant writing the base dir
// past shutdown. Wait is always called, so the direct child is reaped
// before shutdown proceeds.
func stopUncommittedRun(cmd *exec.Cmd) {
	// ESRCH (child already gone, or a test runner without Setpgid) is fine:
	// Wait below still reaps whatever started.
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
	// A no-op when Wait already returned (done is closed); otherwise blocks
	// until the direct child is reaped.
	<-done
}

// waitForRunProcessGroup waits up to timeout for the child's whole process
// group to exit cleanly: the direct child must be reaped (waitDone closed)
// AND the group probe must report no surviving members. Reports true when
// both held within the window, false on timeout. waitDone is nilled after
// it closes so the always-ready channel can't spin the poll loop.
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

// runProcessGroupGone reports whether the child's process group has no live
// members left. Signal 0 probes membership without delivering anything;
// ESRCH means the group is empty (or, for a non-Setpgid test runner, that no
// group led by the child's PID exists — production children always lead
// their own group via defaultCommandRunner's Setpgid, and Wait has already
// reaped such a child by the time the probe's answer is acted on).
func runProcessGroupGone(cmd *exec.Cmd) bool {
	return errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
}

// sweepRunProcessGroup force-kills the child's whole process group (Setpgid
// makes the child its leader) and then waits — polling at
// runGroupPollInterval, bounded by scheduler.DefaultGrace — until the group
// has actually disappeared, reporting whether it did. os/exec's WaitDelay
// SIGKILL hits only the direct child, so the group is swept after every run.
// kill(2) only queues the signal, so the caller must not release the executor
// to the next job (or report the shutdown drain complete) until this
// observation phase returns.
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
