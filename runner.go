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
// group (see withDumbInitInGroup). The context is never cancelled by
// shutdown — the daemon does not abandon an in-flight run — so the only
// cancellation cause is the timeout.
func runRenovateOnce(ctx context.Context, timeout time.Duration, trigger string, repos, env []string, newCmd scheduler.CommandRunner) (ok bool) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := renovateInvocation(repos)

	start := time.Now()
	slog.Info("renovate run starting", "trigger", trigger, "repos", repos, "timeout", timeout)

	cmd := newCmd(runCtx, name, args...)
	cmd.Env = withDumbInitInGroup(env)
	runErr := cmd.Run()
	durationMs := time.Since(start).Milliseconds()

	switch {
	case runErr == nil:
		slog.Info("renovate run complete", "trigger", trigger, "duration_ms", durationMs)
		return true
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// Sweep the child's process group: os/exec's WaitDelay SIGKILL hits
		// only the direct child, and surviving package-manager grandchildren
		// would race the next run against the same base directory.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
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
