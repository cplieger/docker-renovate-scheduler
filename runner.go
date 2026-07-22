package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	scheduler "github.com/cplieger/scheduler/v2"
)

// --- Renovate execution ---

const (
	// renovateEntrypoint is the renovate image's own entrypoint chain
	// (renovate-entrypoint.sh, which exec-chains to the containerbase
	// docker-entrypoint.sh). It establishes the containerbase environment —
	// CONTAINERBASE / CONTAINERBASE_ENV, USER_*, locale, and the
	// ~/.local/bin PATH additions — that Renovate's default
	// binarySource=install needs to install package-manager toolchains (go,
	// npm, etc.) at runtime for lockfile maintenance.
	//
	// Every run is routed through this entrypoint because a run's
	// environment may come from a socket client (a `docker exec` process,
	// which never passed through the image ENTRYPOINT), so the containerbase
	// setup must be re-established per child regardless of what environment
	// the child starts from. The Dockerfile asserts this path exists at
	// build time, so a base-image relocation fails the build, not every run.
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

// defaultCommandRunner builds one Renovate subprocess command: the shared
// scheduler library supplies graceful shutdown (SIGTERM on context
// cancellation, then a DefaultGrace 5s window before SIGKILL), and this
// wrapper streams the child's stdout/stderr straight through to the daemon's
// stdout/stderr — so EVERY run, whichever trigger requested it, lands in the
// container log stream (docker logs → Alloy → Loki) in both scheduling modes.
// Renovate is the logging authority — set LOG_FORMAT=json and let Loki ingest
// its structured output; the scheduler neither captures nor parses it. The
// binary path is the fixed renovateEntrypoint const and the only variable
// args are operator-supplied repo slugs, so there is no untrusted-input
// boundary here.
//
// Setpgid puts the child in its OWN process group. PID 1 is the base image's
// dumb-init, which forwards a `docker stop` SIGTERM to the daemon's entire
// process group — without Setpgid the in-flight Renovate child shares that
// group and is TERMed out-of-band in the same instant as the daemon (exit
// 143), silently defeating the shutdown drain (runCtx's WithoutCancel never
// gets a say). With its own group the child only ever receives signals the
// daemon sends it (the SCHED_TIMEOUT cancellation path), so a container
// recreate mid-run drains the pass to completion as designed, bounded by
// stop_grace_period. Log capture is unaffected: the child inherits the
// daemon's stdout/stderr fds regardless of process group.
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
// forced to 0 (any pre-existing entry is dropped first). The entrypoint chain
// (renovate-entrypoint.sh → containerbase docker-entrypoint.sh) execs a
// nested per-run `dumb-init --`, and default-mode dumb-init forks Renovate
// into a NEW session/process group below the Setpgid group the scheduler
// created. Both escalation stages (the Cancel group SIGTERM and the
// post-timeout group SIGKILL in runRenovateOnce) address only
// -cmd.Process.Pid, so a session-escaped Renovate tree would survive them,
// keep writing the base dir, and overlap the next FIFO job. DUMB_INIT_SETSID=0
// keeps dumb-init in signal-proxy mode without the setsid, so the
// scheduler-created group stays the one containment boundary.
func withDumbInitInGroup(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "DUMB_INIT_SETSID=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "DUMB_INIT_SETSID=0")
}

// runRenovateOnce executes exactly one Renovate pass and reports whether it
// exited cleanly. The pass is bounded by timeout (SCHED_TIMEOUT); on expiry
// the command runner sends SIGTERM with a short grace before SIGKILL. env,
// when non-nil, replaces the child's environment wholesale (a socket client's
// forwarded environment); nil inherits the daemon's — either way with ONE
// scheduler-internal override, DUMB_INIT_SETSID=0, so the nested per-run
// dumb-init cannot detach the Renovate tree from the scheduler's process
// group (see withDumbInitInGroup). ctx (the daemon's uncancellable runCtx)
// never carries shutdown cancellation — the daemon does not abandon a
// committed in-flight run — so the only cancellation cause is the timeout.
//
// shutdownCtx closes the check-then-act window between execute's pre-start
// shutdown check and process creation: a SIGTERM landing in that gap would
// otherwise launch a fresh pass bounded only by SCHED_TIMEOUT, which can
// outlive the container's stop_grace_period and recreate the exit-137 path
// the drain contract exists to prevent. Immediately after Start, shutdownCtx
// is re-checked: if shutdown already won the race, the just-started child is
// reaped (see stopUncommittedRun) and the run reports cancelled=true — logged
// at Warn, never as a run failure — without ever being committed as
// in-flight. Only a run that passes this post-Start handshake drains under
// ctx/SCHED_TIMEOUT.
func runRenovateOnce(ctx, shutdownCtx context.Context, timeout time.Duration, trigger string, repos, env []string, newCmd scheduler.CommandRunner) (ok, cancelled bool) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := renovateInvocation(repos)

	start := time.Now()
	slog.Info("renovate run starting", "trigger", trigger, "repos", repos, "timeout", timeout)

	cmd := newCmd(runCtx, name, args...)
	cmd.Env = withDumbInitInGroup(env)
	if startErr := cmd.Start(); startErr != nil {
		slog.Error("renovate run failed",
			"trigger", trigger, "duration_ms", time.Since(start).Milliseconds(), "error", startErr)
		return false, false
	}
	if shutdownCtx.Err() != nil {
		stopUncommittedRun(cmd)
		slog.Warn("renovate run cancelled by shutdown at start", "trigger", trigger, "repos", repos)
		return false, true
	}
	runErr := cmd.Wait()
	durationMs := time.Since(start).Milliseconds()

	switch {
	case runErr == nil:
		slog.Info("renovate run complete", "trigger", trigger, "duration_ms", durationMs)
		return true, false
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		sweepRunProcessGroup(cmd)
		// The run exceeded SCHED_TIMEOUT. Logged distinctly from a genuine
		// non-zero Renovate exit so operators can tell a slow run from a
		// real failure during triage.
		slog.Error("renovate run timed out",
			"trigger", trigger, "duration_ms", durationMs, "timeout", timeout)
		return false, false
	default:
		// A hard-crashed Renovate (e.g. an OOM-killed node process) exits
		// without reaping its package-manager children. On a normal
		// non-zero exit the group is already empty and the kill is a
		// no-op (ESRCH).
		sweepRunProcessGroup(cmd)
		slog.Error("renovate run failed",
			"trigger", trigger, "duration_ms", durationMs, "error", runErr)
		return false, false
	}
}

// runGroupPollInterval is how often stopUncommittedRun re-probes the child's
// process group for surviving members inside the DefaultGrace window.
const runGroupPollInterval = 50 * time.Millisecond

// stopUncommittedRun reaps a child that started but lost the post-Start
// shutdown handshake in runRenovateOnce: SIGTERM to the child's process
// group (Setpgid makes it the leader, so package-manager grandchildren stop
// with it), a DefaultGrace window for the WHOLE group to exit cleanly, then
// a group SIGKILL sweep if any member lingers. The completion condition is
// the process group's death, not just the direct child's: a leader that
// honors SIGTERM can exit while a package-manager descendant in the same
// group ignores it, and returning on the leader's exit alone would leave
// that descendant writing the base dir past shutdown. Wait is always
// called, so the direct child is reaped before shutdown proceeds.
func stopUncommittedRun(cmd *exec.Cmd) {
	// ESRCH (child already gone, or a test runner without Setpgid) is fine:
	// Wait below still reaps whatever started.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { defer close(done); _ = cmd.Wait() }()

	grace := time.NewTimer(scheduler.DefaultGrace)
	defer grace.Stop()
	poll := time.NewTicker(runGroupPollInterval)
	defer poll.Stop()

	reaped := false
	waitCh := done // nilled after close so the always-ready channel can't spin the loop
	for {
		select {
		case <-waitCh:
			reaped, waitCh = true, nil
			if runProcessGroupGone(cmd) {
				return
			}
		case <-poll.C:
			if reaped && runProcessGroupGone(cmd) {
				return
			}
		case <-grace.C:
			sweepRunProcessGroup(cmd)
			if !reaped {
				<-done
			}
			return
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
// makes the child its leader). os/exec's WaitDelay SIGKILL hits only the
// direct child; surviving package-manager grandchildren would race the next
// FIFO job against the same base directory, so the group is swept after any
// run that did not exit cleanly. No-op when the child never started.
func sweepRunProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
