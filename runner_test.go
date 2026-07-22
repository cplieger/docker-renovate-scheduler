package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	scheduler "github.com/cplieger/scheduler/v2"
	"github.com/cplieger/slogx/capture"
)

func TestRenovateInvocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		repos    []string
		wantArgs []string
	}{
		{"no repos runs configured set", nil, []string{"renovate"}},
		{"single repo passed through", []string{"owner/repo"}, []string{"renovate", "owner/repo"}},
		{"multiple repos passed through", []string{"a/b", "c/d"}, []string{"renovate", "a/b", "c/d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotName, gotArgs := renovateInvocation(tt.repos)
			if gotName != renovateEntrypoint {
				t.Errorf("name = %q, want %q (must route through the image entrypoint)", gotName, renovateEntrypoint)
			}
			if !slices.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}

// recordingRunner returns a scheduler.CommandRunner that records each
// invocation's argument slice (index 0 is the renovate command, the rest are
// repo slugs) and runs the fixed binary bin instead of the real entrypoint
// (absent in the test environment). /usr/bin/true and /usr/bin/false give
// deterministic exit codes. Safe for concurrent use is NOT claimed: callers
// that need cross-goroutine reads synchronize via the daemon's own ordering
// (one executor, sequential runs).
func recordingRunner(bin string, argsLog *[][]string) scheduler.CommandRunner {
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if argsLog != nil {
			*argsLog = append(*argsLog, slices.Clone(args))
		}
		return exec.CommandContext(ctx, bin)
	}
}

// TestRunRenovateOnce_EnvHandling pins the environment contract: a non-nil
// job env REPLACES the child environment wholesale (the forwarded client
// environ, overrides included), and a nil env inherits the daemon's. The
// child is a shell that fails unless the marker variable is present/absent
// as expected, so the assertion is on the child's real environment.
func TestRunRenovateOnce_EnvHandling(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		// script exits 0 iff the environment matches the expectation.
		script string
	}{
		{
			name:   "forwarded env replaces the child environment",
			env:    []string{"RENOVATE_TEST_MARKER=forwarded", "PATH=" + os.Getenv("PATH")},
			script: `[ "$RENOVATE_TEST_MARKER" = "forwarded" ]`,
		},
		{
			name:   "nil env inherits the daemon environment",
			env:    nil,
			script: `[ "$RENOVATE_TEST_MARKER" = "inherited" ]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_TEST_MARKER", "inherited")
			runner := shellAssertRunner(tt.script)
			if ok, _, _ := runRenovateOnce(context.Background(), context.Background(), time.Minute, "test", nil, tt.env, runner); !ok {
				t.Errorf("runRenovateOnce() = false: the child did not see the expected environment (env=%v)", tt.env)
			}
		})
	}
}

func TestDefaultCommandRunner(t *testing.T) {
	t.Parallel()
	cmd := defaultCommandRunner(context.Background(), "echo", "hi")
	if cmd.Stdout != os.Stdout {
		t.Error("Stdout not wired to os.Stdout (Renovate output must stream to the container log)")
	}
	if cmd.Stderr != os.Stderr {
		t.Error("Stderr not wired to os.Stderr")
	}
	if cmd.WaitDelay != 5*time.Second {
		t.Errorf("WaitDelay = %v, want 5s", cmd.WaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel not set (graceful SIGTERM on timeout expected)")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid not set: the child must run in its own process group, or " +
			"dumb-init (PID 1) forwards the docker-stop SIGTERM to the daemon's " +
			"whole group and kills the in-flight run (exit 143), defeating the shutdown drain")
	}
}

// TestDefaultCommandRunner_ChildRunsInOwnProcessGroup proves the OS honors
// Setpgid: a spawned child's process group must differ from the daemon's
// (here: the test process's), so a group-directed SIGTERM at PID 1 cannot
// reach it. This is the behavioral half of the Setpgid pin in
// TestDefaultCommandRunner.
func TestDefaultCommandRunner_ChildRunsInOwnProcessGroup(t *testing.T) {
	t.Parallel()
	cmd := defaultCommandRunner(context.Background(), "sleep", "2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	childPgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(child) failed: %v", err)
	}
	ownPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("Getpgid(self) failed: %v", err)
	}
	if childPgid == ownPgid {
		t.Errorf("child pgid = %d equals parent pgid; child must lead its own process group", childPgid)
	}
	if childPgid != cmd.Process.Pid {
		t.Errorf("child pgid = %d, want %d (the child should lead its own group)", childPgid, cmd.Process.Pid)
	}
}

// TestRunRenovateOnce_TimeoutCancelsRun pins the documented guardrail that a
// wedged run is killed rather than left running into the next request.
func TestRunRenovateOnce_TimeoutCancelsRun(t *testing.T) {
	t.Parallel()
	slowRunner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "5")
	}

	start := time.Now()
	ok, _, _ := runRenovateOnce(context.Background(), context.Background(), 100*time.Millisecond, "test", nil, nil, slowRunner)
	elapsed := time.Since(start)

	if ok {
		t.Error("runRenovateOnce() = true for a run that exceeded the timeout, want false")
	}
	if elapsed > 3*time.Second {
		t.Errorf("runRenovateOnce() returned after %v; the timeout did not cancel the run", elapsed)
	}
}

// TestRunRenovateOnce_EnvForcesDumbInitInGroup pins the one scheduler-
// internal environment override: whatever env a run starts from (nil/ticker
// or a forwarded client environ, even one that tries to re-enable setsid),
// the child sees exactly DUMB_INIT_SETSID=0, so the nested per-run dumb-init
// in the image entrypoint chain stays in signal-proxy mode instead of
// detaching Renovate into a new session the group signals cannot reach.
func TestRunRenovateOnce_EnvForcesDumbInitInGroup(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{"ticker run (nil env inherits daemon environ)", nil},
		{"forwarded env without the variable", []string{"RENOVATE_X=y", "PATH=" + os.Getenv("PATH")}},
		{"forwarded env re-enabling setsid is overridden", []string{"DUMB_INIT_SETSID=1", "PATH=" + os.Getenv("PATH")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DUMB_INIT_SETSID", "1") // daemon environ must be overridden too
			runner := shellAssertRunner(`[ "$DUMB_INIT_SETSID" = "0" ]`)
			if ok, _, _ := runRenovateOnce(context.Background(), context.Background(), time.Minute, "test", nil, tt.env, runner); !ok {
				t.Errorf("runRenovateOnce() = false: child did not see DUMB_INIT_SETSID=0 (env=%v)", tt.env)
			}
		})
	}
}

// TestRunRenovateOnce_TimeoutKillsSessionEscapedDescendants is the process-
// level regression test for the containment boundary: the image entrypoint
// chain ends in a nested per-run dumb-init whose DEFAULT mode forks Renovate
// into a new session/process group, out of reach of both group escalation
// stages (Cancel's SIGTERM and the post-timeout SIGKILL, both aimed at
// -cmd.Process.Pid). The fake entrypoint here models exactly that split: it
// honors DUMB_INIT_SETSID the way dumb-init does — "0" keeps the payload in
// the scheduler-created group, anything else setsids it away. With
// runRenovateOnce forcing DUMB_INIT_SETSID=0, the long-running payload must
// be dead after the timeout; if the override regresses, the payload escapes
// into its own session, survives the sweep, and this test fails.
func TestRunRenovateOnce_TimeoutKillsSessionEscapedDescendants(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable; cannot model dumb-init's session split")
	}
	pidPath := t.TempDir() + "/payload.pid"
	// $1 is pidPath. The payload records its own PID, then blocks well past
	// the run timeout. In-group branch: the payload replaces the entrypoint
	// (stays in the scheduler's group). Default branch: setsid -w detaches
	// it into a new session, exactly like unfixed dumb-init.
	const fakeDumbInit = `if [ "$DUMB_INIT_SETSID" = "0" ]; then
	exec sh -c 'echo $$ > "$0"; exec sleep 30' "$1"
fi
exec setsid -w sh -c 'echo $$ > "$0"; exec sleep 30' "$1"`
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := defaultCommandRunner(ctx, "sh", "-c", fakeDumbInit, "sh", pidPath)
		cmd.Stdout, cmd.Stderr = nil, nil // the payload must not hold the test's stdout pipe
		return cmd
	}

	if ok, _, _ := runRenovateOnce(context.Background(), context.Background(), 500*time.Millisecond, "test", nil, nil, runner); ok {
		t.Fatal("runRenovateOnce() = true for a run that exceeded the timeout, want false")
	}

	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("payload never recorded its PID (entrypoint model did not start): %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad PID file content %q: %v", raw, err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, "a descendant of the run survived the timeout: the process-group containment is broken")
}

// TestRunRenovateOnce_ClassifiesTimeoutAndFailureDistinctly pins the distinct
// ERROR messages for a timed-out run vs a genuine non-zero exit. Both return
// false, so a boolean-only assertion can't tell them apart; alerting keys on
// the message, so a mutation that swaps or downgrades either must fail here.
func TestRunRenovateOnce_ClassifiesTimeoutAndFailureDistinctly(t *testing.T) {
	tests := []struct {
		name    string
		runner  scheduler.CommandRunner
		wantMsg string
		timeout time.Duration
	}{
		{
			name: "timeout logs renovate run timed out",
			runner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sleep", "5")
			},
			timeout: 100 * time.Millisecond,
			wantMsg: "renovate run timed out",
		},
		{
			name:    "non-zero exit logs renovate run failed",
			runner:  func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "false") },
			timeout: time.Minute,
			wantMsg: "renovate run failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			ok, _, _ := runRenovateOnce(context.Background(), context.Background(), tt.timeout, "test", nil, nil, tt.runner)
			if ok {
				t.Errorf("runRenovateOnce() = true, want false")
			}
			if got := rec.CountLevel(slog.LevelError, tt.wantMsg); got != 1 {
				t.Errorf("ERROR records matching %q = %d, want 1; captured: %v", tt.wantMsg, got, rec.Messages())
			}
		})
	}
}

// TestRunRenovateOnce_SuccessLogsCompleteAtInfo pins the success lifecycle
// line's exact message and level. The README's RenovateNoRecentRun deadman
// alert keys on absent_over_time of this exact string, so a silent rewording
// would make that alert fire permanently at the next quiet window; the
// boolean result alone (asserted elsewhere) cannot catch it. Serial: swaps
// slog.Default.
func TestRunRenovateOnce_SuccessLogsCompleteAtInfo(t *testing.T) {
	rec := capture.Default(t)
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "true") }
	ok, cancelled, survived := runRenovateOnce(context.Background(), context.Background(), time.Minute, "test", nil, nil, runner)
	if !ok || cancelled || survived {
		t.Fatalf("runRenovateOnce() = (%v, %v, %v), want (true, false, false)", ok, cancelled, survived)
	}
	if got := rec.CountLevel(slog.LevelInfo, "renovate run complete"); got != 1 {
		t.Errorf("INFO records matching %q = %d, want 1; captured: %v", "renovate run complete", got, rec.Messages())
	}
}

// TestDefaultCommandRunner_CancelSendsSIGTERMNotSIGKILL pins the graceful-
// shutdown guardrail in defaultCommandRunner: on context cancellation the
// Cancel closure sends SIGTERM (not os/exec's default SIGKILL), giving
// Renovate a 5s WaitDelay grace to exit cleanly. A shell that traps SIGTERM
// and exits 42 proves the signal arrived as SIGTERM -- a SIGKILLed process
// cannot run its trap, so its result would be "signal: killed" (ExitCode -1).
// `sleep 30 & wait` makes the trap fire promptly (a foreground sleep would
// defer it until the sleep returned); Stdout/Stderr are detached so the
// backgrounded sleep, reparented when the shell exits, does not hold the test
// process's stdout pipe open and stall `go test`. The child creates a
// readiness marker AFTER installing its trap, and the test polls that
// observable event before cancelling — a fixed sleep would race the trap
// install under load and make a correct runner look like SIGKILL behavior.
func TestDefaultCommandRunner_CancelSendsSIGTERMNotSIGKILL(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	readyPath := t.TempDir() + "/ready"
	cmd := defaultCommandRunner(ctx, "sh", "-c", `trap 'exit 42' TERM; : > "$1"; sleep 30 & wait`, "sh", readyPath)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, "child did not install its SIGTERM trap")
	cancel()

	err := cmd.Wait()
	waited = true
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait() = %v, want *exec.ExitError from the SIGTERM trap (SIGKILL would not run it)", err)
	}
	if exitErr.ExitCode() != 42 {
		t.Errorf("exit code = %d, want 42: the run must receive SIGTERM (graceful) and run its trap, not SIGKILL", exitErr.ExitCode())
	}
}

// TestRunRenovateOnce_ShutdownAtStartCancelsAndReapsChild is the regression
// test for the post-Start shutdown handshake: a SIGTERM landing in the
// check-then-act window between execute's pre-start shutdown check and
// process creation must not commit a fresh pass under the uncancellable run
// context (bounded only by SCHED_TIMEOUT, which can outlive
// stop_grace_period). The injected runner cancels shutdown at process
// creation — after the pre-start check, before Start — so the child starts
// with shutdown already won. runRenovateOnce must report cancelled, reap the
// child promptly (not after its 30s payload or the run timeout), and emit no
// level=ERROR run-failure line: a cancelled start is a Warn, not a failure
// alert. Serial: swaps slog.Default.
func TestRunRenovateOnce_ShutdownAtStartCancelsAndReapsChild(t *testing.T) {
	rec := capture.Default(t)

	shutdownCtx, shutdown := context.WithCancel(context.Background())
	var child *exec.Cmd
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		shutdown() // SIGTERM lands after the pre-start check, before Start
		child = defaultCommandRunner(ctx, "sleep", "30")
		child.Stdout, child.Stderr = nil, nil
		return child
	}

	start := time.Now()
	ok, cancelled, _ := runRenovateOnce(context.Background(), shutdownCtx, time.Minute, "test", nil, nil, runner)
	elapsed := time.Since(start)

	if ok || !cancelled {
		t.Fatalf("runRenovateOnce() = (ok=%v, cancelled=%v), want (false, true)", ok, cancelled)
	}
	if child.ProcessState == nil {
		t.Fatal("child not reaped: Wait never completed on the shutdown-cancelled start")
	}
	if elapsed > 15*time.Second {
		t.Errorf("runRenovateOnce() returned after %v; the cancelled child was not reaped promptly", elapsed)
	}
	if got := rec.CountLevel(slog.LevelError, ""); got != 0 {
		t.Errorf("a shutdown-cancelled start emitted %d ERROR records (false failure alert); captured: %v", got, rec.Messages())
	}
	if got := rec.Count("renovate run cancelled by shutdown at start"); got != 1 {
		t.Errorf("shutdown-cancellation Warn line count = %d, want 1; captured: %v", got, rec.Messages())
	}
}

// TestStopUncommittedRun_SweepsTermIgnoringDescendant is the regression test
// for the partial-group shutdown case: the group leader honors SIGTERM and
// exits promptly while a same-group descendant (a package manager, here a
// TERM-ignoring subshell) survives it. stopUncommittedRun must not return on
// the leader's exit alone — it must keep the DefaultGrace window open for
// the WHOLE group and SIGKILL-sweep the survivors on expiry, or the
// descendant keeps writing the base dir past shutdown. The helper is driven
// directly rather than through runRenovateOnce: the post-Start handshake
// sends SIGTERM microseconds after Start, racing the leader's trap install,
// so the trap-and-survive setup needs a readiness handshake BEFORE the
// signal — the runRenovateOnce routing itself is already pinned by
// TestRunRenovateOnce_ShutdownAtStartCancelsAndReapsChild. The child records
// the descendant's PID before signalling ready; after stopUncommittedRun
// returns, the leader must be reaped and the descendant gone.
func TestStopUncommittedRun_SweepsTermIgnoringDescendant(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	descPath := dir + "/desc.pid"
	readyPath := dir + "/ready"
	// $1 = descendant PID file, $2 = readiness marker. The subshell ignores
	// TERM and respawns its sleep, so only a group SIGKILL removes it; the
	// leader traps TERM and exits promptly (sleep 30 & wait fires the trap
	// as soon as the signal lands).
	script := `( trap '' TERM; while :; do sleep 1; done ) & echo $! > "$1"; trap 'exit 0' TERM; : > "$2"; sleep 30 & wait`
	cmd := defaultCommandRunner(context.Background(), "sh", "-c", script, "sh", descPath, readyPath)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, "child did not finish its trap/descendant setup")
	pidBytes, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatalf("descendant pid not recorded: %v", err)
	}
	descPid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("descendant pid %q not a PID: %v", pidBytes, err)
	}

	start := time.Now()
	stopUncommittedRun(cmd)
	stopped = true
	elapsed := time.Since(start)

	if cmd.ProcessState == nil {
		t.Fatal("leader not reaped: Wait never completed")
	}
	// The SIGKILLed orphan may linger as a zombie until init reaps it; poll
	// briefly rather than asserting instantly.
	waitFor(t, 5*time.Second, func() bool {
		return errors.Is(syscall.Kill(descPid, 0), syscall.ESRCH)
	}, "TERM-ignoring descendant survived stopUncommittedRun's group sweep")
	if elapsed > scheduler.DefaultGrace+10*time.Second {
		t.Errorf("stopUncommittedRun returned after %v; the group sweep must land at grace expiry, not the run timeout", elapsed)
	}
}

// TestRunRenovateOnce_StartFailureIsARunFailureNotAPanic pins the launch
// failure mode: when the child cannot even be started (a missing entrypoint
// binary -- e.g. a base-image relocation that slipped past the Dockerfile's
// build-time assert), runRenovateOnce reports (ok=false, cancelled=false)
// and logs the failure at ERROR, so the executor flips the health marker
// unhealthy and the RenovateRunFailed alert fires, instead of reporting a
// clean run. Serial: swaps slog.Default.
func TestRunRenovateOnce_StartFailureIsARunFailureNotAPanic(t *testing.T) {
	rec := capture.Default(t)

	missing := t.TempDir() + "/no-such-entrypoint"
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, missing)
	}

	ok, cancelled, _ := runRenovateOnce(context.Background(), context.Background(), time.Minute, "test", nil, nil, runner)

	if ok || cancelled {
		t.Fatalf("runRenovateOnce() = (ok=%v, cancelled=%v) for an unstartable child, want (false, false)", ok, cancelled)
	}
	if got := rec.CountLevel(slog.LevelError, "renovate run failed"); got != 1 {
		t.Errorf("ERROR records matching the run-failure line = %d, want 1; captured: %v", got, rec.Messages())
	}
}

// TestDefaultCommandRunner_CancelOnExitedChildReportsProcessDone pins the
// Cancel closure's ESRCH mapping: when the child's process group is already
// gone by the time the context fires (the run finished in the same instant
// the timeout expired), Cancel must report os.ErrProcessDone -- which
// os/exec treats as "nothing to cancel" -- rather than a raw ESRCH error
// that cmd.Wait would surface, misreporting a clean run as failed.
func TestDefaultCommandRunner_CancelOnExitedChildReportsProcessDone(t *testing.T) {
	t.Parallel()
	cmd := defaultCommandRunner(context.Background(), "true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	// The child led its own group (Setpgid) and is reaped, so the group is
	// empty: the Cancel closure's group SIGTERM gets ESRCH.
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Cancel() on an exited child = %v, want os.ErrProcessDone (ESRCH must map to process-done, not surface as a cancel failure)", err)
	}
}

// TestStopUncommittedRun_ReturnsPromptlyWhenGroupDiesWithinGrace pins the
// poll path: the leader honors SIGTERM and exits at once, a TERM-ignoring
// descendant exits on its own well inside the DefaultGrace window -- so
// stopUncommittedRun must return as soon as the poll observes the empty
// group instead of sitting out the full grace and SIGKILL-sweeping
// processes that already exited (which would stall every shutdown in this
// window by the whole grace period).
func TestStopUncommittedRun_ReturnsPromptlyWhenGroupDiesWithinGrace(t *testing.T) {
	t.Parallel()
	readyPath := t.TempDir() + "/ready"
	// The descendant ignores TERM (only its current sleep dies with the
	// group signal; the loop respawns it) and exits by itself after ~0.8s;
	// the leader traps TERM and exits immediately (sleep 30 & wait fires
	// the trap as soon as the signal lands).
	script := `( trap '' TERM; i=0; while [ $i -lt 8 ]; do sleep 0.1; i=$((i+1)); done ) & trap 'exit 0' TERM; : > "$1"; sleep 30 & wait`
	cmd := defaultCommandRunner(context.Background(), "sh", "-c", script, "sh", readyPath)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, "child did not finish its setup")

	start := time.Now()
	stopUncommittedRun(cmd)
	stopped = true
	elapsed := time.Since(start)

	if cmd.ProcessState == nil {
		t.Fatal("leader not reaped: Wait never completed")
	}
	if elapsed >= scheduler.DefaultGrace {
		t.Errorf("stopUncommittedRun returned after %v; the group died within ~0.8s, so the poll must return promptly instead of waiting out the full %v grace", elapsed, scheduler.DefaultGrace)
	}
}

// TestStopUncommittedRun_SweepsLeaderThatIgnoresTermAtGraceExpiry pins the
// grace-expiry path for an unreaped leader: a leader that ignores SIGTERM
// outright is force-killed by the group sweep when DefaultGrace expires,
// and stopUncommittedRun still waits for Wait to reap it before returning
// -- a return without the reap leaves a zombie and races the daemon's
// shutdown against the child's exit.
func TestStopUncommittedRun_SweepsLeaderThatIgnoresTermAtGraceExpiry(t *testing.T) {
	t.Parallel()
	readyPath := t.TempDir() + "/ready"
	// The leader ignores TERM and respawns its sleep forever; only the
	// grace-expiry group SIGKILL removes it.
	script := `trap '' TERM; : > "$1"; while :; do sleep 1; done`
	cmd := defaultCommandRunner(context.Background(), "sh", "-c", script, "sh", readyPath)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, "leader did not install its TERM ignore")

	start := time.Now()
	stopUncommittedRun(cmd)
	stopped = true
	elapsed := time.Since(start)

	if cmd.ProcessState == nil {
		t.Fatal("leader not reaped after the grace-expiry sweep: Wait never completed")
	}
	if elapsed < scheduler.DefaultGrace {
		t.Errorf("stopUncommittedRun returned after %v, before the %v grace expired; a TERM-ignoring leader can only die via the expiry sweep", elapsed, scheduler.DefaultGrace)
	}
	if elapsed > scheduler.DefaultGrace+10*time.Second {
		t.Errorf("stopUncommittedRun returned after %v; the sweep must land at grace expiry", elapsed)
	}
}

// TestRunRenovateOnce_TimeoutSweepObservesGroupDeath is the regression test
// for the sweep's observation phase: kill(2) only queues SIGKILL, so a
// forced cleanup that returns on signal submission alone can release the
// executor to the next FIFO job while a package-manager descendant is still
// alive against the same base directory. The leader spawns a TERM-ignoring
// descendant (models a package manager that shrugs off the Cancel SIGTERM)
// and then outlives the run timeout; when runRenovateOnce returns, the
// descendant must ALREADY be gone — no post-return polling window — because
// sweepRunProcessGroup must not return until the whole group has died.
func TestRunRenovateOnce_TimeoutSweepObservesGroupDeath(t *testing.T) {
	t.Parallel()
	descPath := t.TempDir() + "/desc.pid"
	// $1 is descPath. The backgrounded subshell ignores TERM and respawns
	// its sleep forever; only the sweep's group SIGKILL removes it. The
	// leader records the descendant's PID, then blocks past the timeout.
	script := `( trap '' TERM; while :; do sleep 1; done ) & echo $! > "$1"; sleep 30 & wait`
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := defaultCommandRunner(ctx, "sh", "-c", script, "sh", descPath)
		cmd.Stdout, cmd.Stderr = nil, nil // the descendant must not hold the test's stdout pipe
		return cmd
	}

	ok, _, groupSurvived := runRenovateOnce(context.Background(), context.Background(), 500*time.Millisecond, "test", nil, nil, runner)
	if ok {
		t.Fatal("runRenovateOnce() = true for a run that exceeded the timeout, want false")
	}
	if groupSurvived {
		t.Error("runRenovateOnce() reported groupSurvived=true although the sweep confirmed the group's death")
	}

	raw, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatalf("descendant PID never recorded (leader did not start): %v", err)
	}
	descPid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad descendant PID file content %q: %v", raw, err)
	}
	if !errors.Is(syscall.Kill(descPid, 0), syscall.ESRCH) {
		t.Error("TERM-ignoring descendant still alive after runRenovateOnce returned: " +
			"the sweep released the executor before observing the group's death")
	}
}

// TestWithDumbInitInGroup pins the exactly-one-entry contract at the slice
// level: the pre-existing DUMB_INIT_SETSID entry is DROPPED, not merely
// overridden by appending. dumb-init is a C program whose getenv returns the
// FIRST match in environ, while the shells the indirect tests use resolve
// duplicates last-wins — so a drop-regression (append without drop) passes
// every existing shell-assertion test yet lets the client's =1 win inside
// dumb-init, silently reopening the session escape.
func TestWithDumbInitInGroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "appends the override to a forwarded env without the variable",
			env:  []string{"PATH=/usr/bin", "RENOVATE_X=y"},
			want: []string{"PATH=/usr/bin", "RENOVATE_X=y", "DUMB_INIT_SETSID=0"},
		},
		{
			name: "drops a pre-existing entry so exactly one remains",
			env:  []string{"DUMB_INIT_SETSID=1", "PATH=/usr/bin"},
			want: []string{"PATH=/usr/bin", "DUMB_INIT_SETSID=0"},
		},
		{
			name: "drops every duplicate pre-existing entry",
			env:  []string{"DUMB_INIT_SETSID=1", "PATH=/usr/bin", "DUMB_INIT_SETSID="},
			want: []string{"PATH=/usr/bin", "DUMB_INIT_SETSID=0"},
		},
		{
			name: "empty non-nil env still gets the override",
			env:  []string{},
			want: []string{"DUMB_INIT_SETSID=0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := withDumbInitInGroup(tt.env); !slices.Equal(got, tt.want) {
				t.Errorf("withDumbInitInGroup(%v) = %v, want %v (getenv returns the FIRST environ match: a leftover client entry would beat the appended override inside dumb-init)", tt.env, got, tt.want)
			}
		})
	}
}

// TestWithDumbInitInGroup_NilEnvUsesDaemonEnviron pins the nil branch at the
// slice level: the daemon's own environ is inherited, its DUMB_INIT_SETSID
// is dropped, and exactly one =0 entry remains. Not parallel: t.Setenv.
func TestWithDumbInitInGroup_NilEnvUsesDaemonEnviron(t *testing.T) {
	t.Setenv("DUMB_INIT_SETSID", "1")
	t.Setenv("RENOVATE_TEST_MARKER", "daemon")

	got := withDumbInitInGroup(nil)

	entries := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "DUMB_INIT_SETSID=") {
			entries++
			if kv != "DUMB_INIT_SETSID=0" {
				t.Errorf("DUMB_INIT_SETSID entry = %q, want DUMB_INIT_SETSID=0", kv)
			}
		}
	}
	if entries != 1 {
		t.Errorf("DUMB_INIT_SETSID entries = %d, want exactly 1 (dumb-init's getenv takes the first match, so a duplicate is a containment hazard)", entries)
	}
	if !slices.Contains(got, "RENOVATE_TEST_MARKER=daemon") {
		t.Error("daemon environ not inherited for a nil env")
	}
}

// TestSweepRunProcessGroup_NeverStartedChildIsNothingToSweep covers the
// documented never-started contract: a cmd whose Start was never called (or
// failed) has no process group, so the sweep reports true immediately
// instead of dereferencing a nil Process or waiting out the grace window.
func TestSweepRunProcessGroup_NeverStartedChildIsNothingToSweep(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true") // never started: cmd.Process == nil

	start := time.Now()
	if !sweepRunProcessGroup(cmd) {
		t.Error("sweepRunProcessGroup() = false for a never-started child, want true (nothing to sweep)")
	}
	if elapsed := time.Since(start); elapsed >= scheduler.DefaultGrace {
		t.Errorf("sweepRunProcessGroup returned after %v for a never-started child; it must return immediately, not wait out the %v grace", elapsed, scheduler.DefaultGrace)
	}
}

// TestSweepRunGroupOrWarn_UnconfirmableGroupDeathReportsSurvived drives the
// survived branch of sweepRunGroupOrWarn through the REAL function: the
// group leader is SIGKILLed but deliberately NOT reaped (no Wait), so the
// zombie keeps its process group registered and the sweep's group probe
// reports live members for the whole bounded window -- the same observable
// state as a group whose death cannot be confirmed. sweepRunGroupOrWarn
// must report survived=true (the executor's fatal containment signal) and
// log the caller's message at Warn with the pid. Serial: swaps slog.Default.
func TestSweepRunGroupOrWarn_UnconfirmableGroupDeathReportsSurvived(t *testing.T) {
	rec := capture.Default(t)

	cmd := defaultCommandRunner(context.Background(), "sleep", "30")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	// Kill the leader without reaping it: the unreaped zombie holds the
	// process group open, so runProcessGroupGone stays false through the
	// sweep's entire DefaultGrace window.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	survived := sweepRunGroupOrWarn(cmd, "run process group survived the kill sweep (test)", "test")

	if !survived {
		t.Error("sweepRunGroupOrWarn() = false for a group the sweep cannot confirm dead, want true (the fatal containment signal must fire)")
	}
	if got := rec.CountLevel(slog.LevelWarn, "run process group survived the kill sweep (test)"); got != 1 {
		t.Errorf("Warn records matching the survival message = %d, want 1; captured: %v", got, rec.Messages())
	}
}
