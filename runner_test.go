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

	"github.com/cplieger/scheduler/v4"
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
			name:   "forwarded env does not merge omitted daemon variables",
			env:    []string{"RENOVATE_TEST_MARKER=forwarded", "PATH=" + os.Getenv("PATH")},
			script: `[ -z "${RENOVATE_DAEMON_ONLY+x}" ]`,
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
			t.Setenv("RENOVATE_DAEMON_ONLY", "daemon")
			runner := shellAssertRunner(tt.script)
			if got := runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test", runPayload{Env: tt.env}, runner); got != runComplete {
				t.Errorf("runRenovateOnce() = %v, want runComplete: the child did not see the expected environment (env=%v)", got, tt.env)
			}
		})
	}
}

func TestDefaultCommandRunner(t *testing.T) {
	t.Parallel()
	cmd := defaultCommandRunner(t.Context(), "echo", "hi")
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
	cmd := defaultCommandRunner(t.Context(), "sleep", "2")
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
			if got := runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test", runPayload{Env: tt.env}, runner); got != runComplete {
				t.Errorf("runRenovateOnce() = %v, want runComplete: child did not see DUMB_INIT_SETSID=0 (env=%v)", got, tt.env)
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

	if got := runRenovateOnce(t.Context(), t.Context().Err, 500*time.Millisecond, "test", runPayload{}, runner); got == runComplete {
		t.Fatal("runRenovateOnce() = runComplete for a run that exceeded the timeout, want a failure outcome")
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
// ERROR messages for a timed-out run vs a genuine non-zero exit. Both report
// the same outcome, so the outcome alone can't tell them apart; alerting keys on
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
			got := runRenovateOnce(t.Context(), t.Context().Err, tt.timeout, "test", runPayload{}, tt.runner)
			if got == runComplete {
				t.Errorf("runRenovateOnce() = runComplete, want a failure outcome")
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
// outcome alone (asserted elsewhere) cannot catch it. Serial: swaps
// slog.Default.
func TestRunRenovateOnce_SuccessLogsCompleteAtInfo(t *testing.T) {
	rec := capture.Default(t)
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "true") }
	got := runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test", runPayload{}, runner)
	if got != runComplete {
		t.Fatalf("runRenovateOnce() = %v, want runComplete", got)
	}
	if got := rec.CountLevel(slog.LevelInfo, "renovate run complete"); got != 1 {
		t.Errorf("INFO records matching %q = %d, want 1; captured: %v", "renovate run complete", got, rec.Messages())
	}
}

// TestRunRenovateOnce_CleanRunSweepsLeftoverGroupMember pins the containment
// sweep on the SUCCESS arm. A zero exit is no proof the run's process group is
// empty: the leader here exits cleanly while a TERM-ignoring member of its
// group is still alive, modelling a package manager that outlives Renovate. The
// member must be gone by the time runRenovateOnce returns, because the executor
// is released to the next FIFO job against the same base directory the instant
// it does.
func TestRunRenovateOnce_CleanRunSweepsLeftoverGroupMember(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidPath := dir + "/leader.pid"
	proceedPath := dir + "/proceed"
	// The leader publishes its own pid (which is its pgid, via Setpgid) so the
	// test can join a member to that group, then blocks until released and
	// exits zero.
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := defaultCommandRunner(ctx, "sh", "-c",
			`echo $$ > "$1"; until [ -e "$2" ]; do sleep 0.05; done`, "sh", pidPath, proceedPath)
		cmd.Stdout, cmd.Stderr = nil, nil // the member must not hold the test's stdout pipe
		return cmd
	}

	resultCh := make(chan runOutcome, 1)
	go func() {
		resultCh <- runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test", runPayload{}, runner)
	}()

	waitFor(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(pidPath)
		return err == nil && len(strings.TrimSpace(string(raw))) > 0
	}, "the run leader never published its pid")
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read the leader pid file: %v", err)
	}
	leaderPid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad leader PID file content %q: %v", raw, err)
	}
	member := startTermIgnoringGroupMember(t, leaderPid)
	if err := os.WriteFile(proceedPath, nil, 0o600); err != nil {
		t.Fatalf("release the run leader: %v", err)
	}

	got := <-resultCh
	// A clean pass stays a success: the point is that the leftover group
	// member is reaped BEFORE the executor is released to the next job, not
	// that a completed run is reported failed.
	if got != runComplete {
		t.Fatalf("runRenovateOnce() = %v, want runComplete: a zero-exiting leader whose live group member was swept is still a successful run", got)
	}
	member.awaitReap(t)
	if got := member.exitSignal(); got != syscall.SIGKILL {
		t.Errorf("leftover group member exited on signal %v, want SIGKILL: a clean run must still sweep its process group", got)
	}
}

// TestRunRenovateOnce_EnvironmentValuesNeverReachLifecycleLogs pins the
// credential boundary on the run's own lifecycle records: the forwarded
// environment can carry RENOVATE_TOKEN, so no value from it may appear in a
// message or an attribute. Serial: swaps slog.Default.
func TestRunRenovateOnce_EnvironmentValuesNeverReachLifecycleLogs(t *testing.T) {
	const secret = "test-only-renovate-token-7f3c9d"
	rec := capture.Default(t)
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}

	got := runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "external",
		runPayload{Repos: []string{"owner/repo"}, Env: []string{"RENOVATE_TOKEN=" + secret}}, runner)

	if got != runComplete {
		t.Fatalf("runRenovateOnce() = %v, want runComplete", got)
	}
	if key, value, found := leakedRecord(rec, secret); found {
		t.Errorf("runRenovateOnce() leaked a forwarded environment value in %s = %q", key, value)
	}
}

// TestRunRenovateOnce_LifecycleRecordsCarryCorrelationAttributes pins the
// property every arm shares: each pass emits exactly one "renovate run
// starting" record carrying the attributes that identify the run (trigger,
// repo scope, timeout) and exactly one terminal record carrying its trigger
// and elapsed time, so an operator can tie a terminal line back to its start.
// The per-arm specifics (the timeout attribute on a timed-out run, the error
// attribute on a failure) belong to the focused tests above. Serial: swaps
// slog.Default.
func TestRunRenovateOnce_LifecycleRecordsCarryCorrelationAttributes(t *testing.T) {
	tests := []struct {
		name          string
		trigger       string
		repos         []string
		timeout       time.Duration
		runner        scheduler.CommandRunner
		terminalMsg   string
		terminalLevel slog.Level
	}{
		{
			name:          "clean run",
			trigger:       "startup",
			repos:         []string{"owner/repo"},
			timeout:       time.Minute,
			runner:        func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "true") },
			terminalMsg:   "renovate run complete",
			terminalLevel: slog.LevelInfo,
		},
		{
			name:    "timed-out run",
			trigger: "interval",
			repos:   []string{"owner/slow"},
			timeout: 100 * time.Millisecond,
			runner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sleep", "5")
			},
			terminalMsg:   "renovate run timed out",
			terminalLevel: slog.LevelError,
		},
		{
			name:          "non-zero exit",
			trigger:       "external",
			repos:         []string{"owner/broken"},
			timeout:       time.Minute,
			runner:        func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "false") },
			terminalMsg:   "renovate run failed",
			terminalLevel: slog.LevelError,
		},
		{
			name:    "start failure",
			trigger: "external",
			repos:   []string{"owner/unstartable"},
			timeout: time.Minute,
			runner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "/nonexistent/renovate-entrypoint.sh")
			},
			terminalMsg:   "renovate run failed",
			terminalLevel: slog.LevelError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)

			_ = runRenovateOnce(t.Context(), t.Context().Err, tt.timeout, tt.trigger,
				runPayload{Repos: tt.repos}, tt.runner)

			if got := rec.CountExact("renovate run starting"); got != 1 {
				t.Fatalf("records matching %q = %d, want 1; captured: %v", "renovate run starting", got, rec.Messages())
			}
			if got := rec.CountExact(tt.terminalMsg); got != 1 {
				t.Fatalf("records matching %q = %d, want 1; captured: %v", tt.terminalMsg, got, rec.Messages())
			}
			if got, ok := rec.AttrValueExact("renovate run starting", "trigger"); !ok || got != tt.trigger {
				t.Errorf("start record trigger = (%q, %v), want (%q, true)", got, ok, tt.trigger)
			}
			if got, ok := rec.Attr("renovate run starting", "repos"); !ok || !slices.Equal(reposAttr(t, got), tt.repos) {
				t.Errorf("start record repos = (%v, %v), want %v", got, ok, tt.repos)
			}
			timeoutAttr, ok := rec.Attr("renovate run starting", "timeout")
			if !ok || timeoutAttr.Kind() != slog.KindDuration || timeoutAttr.Duration() != tt.timeout {
				t.Errorf("start record timeout = (%v, %v), want the %v duration", timeoutAttr, ok, tt.timeout)
			}
			if got := rec.CountLevel(tt.terminalLevel, tt.terminalMsg); got != 1 {
				t.Errorf("%s records matching %q = %d, want 1; captured: %v", tt.terminalLevel, tt.terminalMsg, got, rec.Messages())
			}
			if got, ok := rec.AttrValueExact(tt.terminalMsg, "trigger"); !ok || got != tt.trigger {
				t.Errorf("terminal record trigger = (%q, %v), want (%q, true)", got, ok, tt.trigger)
			}
			durationAttr, ok := rec.Attr(tt.terminalMsg, "duration_ms")
			if !ok || durationAttr.Kind() != slog.KindInt64 {
				t.Errorf("terminal record duration_ms = (%v, %v), want an int64 attribute", durationAttr, ok)
			}
		})
	}
}

// reposAttr reads a repo-scope attribute back as a string slice.
func reposAttr(t *testing.T, value slog.Value) []string {
	t.Helper()
	repos, ok := value.Any().([]string)
	if !ok {
		t.Fatalf("repos attribute is %T, want []string", value.Any())
	}
	return repos
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
	ctx, cancel := context.WithCancel(t.Context())
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
// context (bounded only by RUN_TIMEOUT, which can outlive
// stop_grace_period). The injected runner cancels shutdown at process
// creation — after the pre-start check, before Start — so the child starts
// with shutdown already won. runRenovateOnce must report runCancelled, reap the
// child promptly (not after its 30s payload or the run timeout), and emit no
// level=ERROR run-failure line: a cancelled start is a Warn, not a failure
// alert. Serial: swaps slog.Default.
func TestRunRenovateOnce_ShutdownAtStartCancelsAndReapsChild(t *testing.T) {
	rec := capture.Default(t)

	shutdownCtx, shutdown := context.WithCancel(t.Context())
	var child *exec.Cmd
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		shutdown() // SIGTERM lands after the pre-start check, before Start
		child = defaultCommandRunner(ctx, "sleep", "30")
		child.Stdout, child.Stderr = nil, nil
		return child
	}

	start := time.Now()
	got := runRenovateOnce(t.Context(), shutdownCtx.Err, time.Minute, "test", runPayload{}, runner)
	elapsed := time.Since(start)

	if got != runCancelled {
		t.Fatalf("runRenovateOnce() = %v, want runCancelled", got)
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

// groupMember is a SIGTERM-ignoring process that has JOINED the runner child's
// process group while remaining a direct child of the TEST binary, so the test
// itself reaps it. A reaper goroutine collects it the instant it dies and
// records when, which is the instant the process group actually became empty.
type groupMember struct {
	pid    int
	cmd    *exec.Cmd
	reaped chan time.Time
}

// startTermIgnoringGroupMember adds a SIGTERM-ignoring process to pgid's
// process group, modelling the package-manager grandchild that shrugs off the
// runner's group SIGTERM and has to be SIGKILL-swept. Fixture strategy (why
// it is a child of the test binary): startGroupMember.
func startTermIgnoringGroupMember(t *testing.T, pgid int) *groupMember {
	t.Helper()
	// exec replaces the shell, so the surviving process has no children of
	// its own to orphan, and an ignored disposition survives exec, so the
	// sleep inherits the TERM ignore.
	return startGroupMember(t, pgid, `trap '' TERM; : > "$1"; exec sleep 30`,
		"group member never installed its SIGTERM ignore")
}

// startTermHonoringGroupMember adds a process with the DEFAULT SIGTERM
// disposition to pgid's process group, modelling the package-manager
// grandchild that stops as soon as the run's group SIGTERM reaches it. Which
// signal actually reached it is then readable from its exit signal: SIGTERM
// when the whole group was addressed, SIGKILL when only the leader was and
// the member had to be force-swept afterwards.
func startTermHonoringGroupMember(t *testing.T, pgid int) *groupMember {
	t.Helper()
	// exec replaces the shell, so the surviving process has no children of
	// its own to orphan and takes the default SIGTERM disposition.
	return startGroupMember(t, pgid, `: > "$1"; exec sleep 30`,
		"group member never started")
}

// startGroupMember starts `sh -c script` as a direct child of the TEST binary
// joined into pgid's process group, waits for the readiness file the script
// creates at "$1" (failing with readyMsg if it never appears), and asserts
// the join. A reaper goroutine collects the member the instant it dies and
// records when.
//
// It joins the group as a direct child of the TEST binary rather than being
// forked as a descendant of the runner's own child, and that is what makes
// every group-death assertion here deterministic. A descendant is orphaned the
// moment the group leader exits and reparents to PID 1 — and a zombie is STILL
// a process-group member as far as kill(2) is concerned, so
// runProcessGroupGone cannot see the group empty until the ambient reaper
// collects it. That probe IS the runner's group-death observation, so a fixture
// built on an orphaned descendant measures the ambient reaper instead of the
// code under test: it fails outright wherever PID 1 does not reap (any
// app-as-PID-1 container) and flakes on a loaded CI runner, where the reap can
// miss the bounded window. A direct child is reaped here, at a known instant.
func startGroupMember(t *testing.T, pgid int, script, readyMsg string) *groupMember {
	t.Helper()
	readyPath := t.TempDir() + "/member-ready"
	cmd := exec.Command("sh", "-c", script, "sh", readyPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start group member: %v", err)
	}
	m := &groupMember{pid: cmd.Process.Pid, cmd: cmd, reaped: make(chan time.Time, 1)}
	go func() {
		_ = cmd.Wait()
		m.reaped <- time.Now()
	}()
	t.Cleanup(func() { _ = syscall.Kill(m.pid, syscall.SIGKILL) })

	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, readyMsg)
	// Guard against a vacuous fixture: a member that failed to join the group
	// leaves the group empty as soon as the runner's child is reaped, and
	// every assertion downstream then passes for the wrong reason.
	if got, err := syscall.Getpgid(m.pid); err != nil || got != pgid {
		t.Fatalf("group member pgid = (%d, %v), want (%d, nil)", got, err, pgid)
	}
	return m
}

// awaitReap waits for the reaper goroutine to collect the member and returns
// the instant it did, which is also when the process group emptied in every
// fixture whose member outlives the runner's own child.
func (m *groupMember) awaitReap(t *testing.T) time.Time {
	t.Helper()
	select {
	case at := <-m.reaped:
		return at
	case <-time.After(30 * time.Second):
		t.Fatal("group member was never reaped")
		return time.Time{}
	}
}

// exitSignal reports the signal that killed the member, or 0 if it exited on
// its own. Valid only after awaitReap (the channel receive orders the
// ProcessState write from the reaper goroutine before this read).
func (m *groupMember) exitSignal() syscall.Signal {
	ws, ok := m.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0
	}
	return ws.Signal()
}

// startTermHonoringChild starts the stand-in for the runner's own Renovate
// child: a bare sleep, which takes the default SIGTERM disposition (so the
// group TERM removes it at once) and forks no children of its own to orphan.
// Returns the command and a func the test calls once stopUncommittedRun has
// reaped it, so an early t.Fatal cannot leak the group.
func startTermHonoringChild(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	// context.Background() (not t.Context()): the child is reaped in t.Cleanup, which runs after t.Context() would already have cancelled it.
	cmd := defaultCommandRunner(context.Background(), "sleep", "30")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	return cmd, func() { reaped = true }
}

// TestStopUncommittedRun_SweepsTermIgnoringGroupMember is the regression test
// for the partial-group shutdown case: the runner's own child honors SIGTERM
// and is reaped promptly while a same-group process (a package manager, here a
// TERM-ignoring sleep) survives it. stopUncommittedRun must not return on the
// child's exit alone — it must hold the DefaultGrace window open for the WHOLE
// group and SIGKILL-sweep the survivors on expiry, or the survivor keeps
// writing the base dir past shutdown. The helper is driven directly rather
// than through runRenovateOnce: the post-Start handshake sends SIGTERM
// microseconds after Start, so the survive-the-TERM setup needs to be in place
// BEFORE the signal — the runRenovateOnce routing itself is already pinned by
// TestRunRenovateOnce_ShutdownAtStartCancelsAndReapsChild.
func TestStopUncommittedRun_SweepsTermIgnoringGroupMember(t *testing.T) {
	t.Parallel()
	cmd, markReaped := startTermHonoringChild(t)
	member := startTermIgnoringGroupMember(t, cmd.Process.Pid)

	start := time.Now()
	stopUncommittedRun(cmd)
	markReaped()
	elapsed := time.Since(start)

	if cmd.ProcessState == nil {
		t.Fatal("runner child not reaped: Wait never completed")
	}
	member.awaitReap(t)
	if got := member.exitSignal(); got != syscall.SIGKILL {
		t.Errorf("TERM-ignoring group member exited on signal %v, want SIGKILL: the grace-expiry sweep must force-kill the whole group", got)
	}
	if elapsed < scheduler.DefaultGrace {
		t.Errorf("stopUncommittedRun returned after %v, inside the %v grace: it must not return on the direct child's exit while the group still has live members", elapsed, scheduler.DefaultGrace)
	}
	if elapsed > scheduler.DefaultGrace+10*time.Second {
		t.Errorf("stopUncommittedRun returned after %v; the group sweep must land at grace expiry, not the run timeout", elapsed)
	}
}

// TestStopUncommittedRun_TermsTheWholeGroupNotJustTheLeader pins the target of
// the shutdown SIGTERM: it addresses the run's process GROUP, so a
// package-manager process beside the leader gets the same chance to stop
// cleanly. A signal aimed at the leader alone leaves that process running
// until the grace-expiry force-kill, which is both a slower shutdown and a
// package manager killed mid-write against the base directory.
//
// Read from the member's exit signal, not from timing: SIGTERM means the group
// TERM reached it, SIGKILL means only the sweep did.
func TestStopUncommittedRun_TermsTheWholeGroupNotJustTheLeader(t *testing.T) {
	t.Parallel()
	cmd, markReaped := startTermHonoringChild(t)
	member := startTermHonoringGroupMember(t, cmd.Process.Pid)

	stopUncommittedRun(cmd)
	markReaped()

	if cmd.ProcessState == nil {
		t.Fatal("runner child not reaped: Wait never completed")
	}
	member.awaitReap(t)
	if got := member.exitSignal(); got != syscall.SIGTERM {
		t.Errorf("group member exit signal = %v, want SIGTERM: the shutdown TERM must address the run's whole process group, not the leader alone", got)
	}
}

// TestRunRenovateOnce_StartFailureIsARunFailureNotAPanic pins the launch
// failure mode: when the child cannot even be started (a missing entrypoint
// binary -- e.g. a base-image relocation that slipped past the Dockerfile's
// build-time assert), runRenovateOnce reports runFailed
// and logs the failure at ERROR, so the executor flips the health marker
// unhealthy and the RenovateRunFailed alert fires, instead of reporting a
// clean run. Serial: swaps slog.Default.
func TestRunRenovateOnce_StartFailureIsARunFailureNotAPanic(t *testing.T) {
	rec := capture.Default(t)

	missing := t.TempDir() + "/no-such-entrypoint"
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, missing)
	}

	got := runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test", runPayload{}, runner)

	if got != runFailed {
		t.Fatalf("runRenovateOnce() = %v for an unstartable child, want runFailed", got)
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
	cmd := defaultCommandRunner(t.Context(), "true")
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

// TestDefaultCommandRunner_CancelSignalsTheWholeProcessGroup pins the target of
// the Cancel closure's SIGTERM: the run's process GROUP, so the package-manager
// toolchains Renovate installs at runtime stop with it instead of continuing to
// write the base directory after the run timeout fired. os/exec's own WaitDelay
// SIGKILL reaches the direct child only, so a signal aimed at the leader alone
// leaves those processes for the post-timeout sweep — or, on the paths that do
// not sweep, running against the next FIFO job's base directory.
//
// Read from the member's exit signal rather than from timing: SIGTERM means the
// group was addressed, and a member that is never signalled at all outlives the
// cancel entirely.
func TestDefaultCommandRunner_CancelSignalsTheWholeProcessGroup(t *testing.T) {
	t.Parallel()
	cmd, markReaped := startTermHonoringChild(t)
	member := startTermHonoringGroupMember(t, cmd.Process.Pid)

	if err := cmd.Cancel(); err != nil {
		t.Fatalf("Cancel() = %v, want nil for a live run group", err)
	}
	_ = cmd.Wait()
	markReaped()

	member.awaitReap(t)
	if got := member.exitSignal(); got != syscall.SIGTERM {
		t.Errorf("group member exit signal = %v, want SIGTERM: Cancel must address the run's whole process group, not the leader alone", got)
	}
}

// TestStopUncommittedRun_ReturnsPromptlyWhenGroupDiesWithinGrace pins the poll
// path: the runner's own child honors SIGTERM and is reaped at once, a
// TERM-ignoring group member outlives it and then exits well inside the
// DefaultGrace window — so stopUncommittedRun must return as soon as the poll
// observes the empty group instead of sitting out the full grace and
// SIGKILL-sweeping processes that already exited (which would stall every
// shutdown in this window by the whole grace period).
//
// The member's death is triggered from HERE, so the test knows exactly when the
// group emptied and asserts promptness relative to that instant. The earlier
// fixture drove the timing from a `sleep` forked inside the child and compared
// only against DefaultGrace, which made a slow fork/exec under full-suite load
// indistinguishable from the bug — it flaked in CI for exactly that reason.
func TestStopUncommittedRun_ReturnsPromptlyWhenGroupDiesWithinGrace(t *testing.T) {
	t.Parallel()
	cmd, markReaped := startTermHonoringChild(t)
	member := startTermIgnoringGroupMember(t, cmd.Process.Pid)

	// Outlive the poll's first ticks, then die: far enough inside the grace
	// that a prompt return is unambiguous, and driven by an in-process timer
	// rather than a forked child's wall-clock deadline. Stopped on cleanup
	// (LIFO, so before the member's own kill) in case an assertion above
	// aborts the test first — a late fire could otherwise signal a recycled
	// PID belonging to another test.
	const memberLifetime = 250 * time.Millisecond
	kill := time.AfterFunc(memberLifetime, func() { _ = syscall.Kill(member.pid, syscall.SIGKILL) })
	t.Cleanup(func() { kill.Stop() })

	start := time.Now()
	stopUncommittedRun(cmd)
	markReaped()
	elapsed := time.Since(start)
	emptiedAt := member.awaitReap(t).Sub(start)

	if cmd.ProcessState == nil {
		t.Fatal("runner child not reaped: Wait never completed")
	}
	if emptiedAt >= scheduler.DefaultGrace {
		t.Fatalf("group only emptied %v after the stop, at or past the %v grace: this fixture cannot tell a prompt return from a swept one, so the promptness assertion below is meaningless", emptiedAt, scheduler.DefaultGrace)
	}
	if elapsed >= scheduler.DefaultGrace {
		t.Errorf("stopUncommittedRun returned after %v; the group emptied at %v, so the poll must return promptly instead of waiting out the full %v grace", elapsed, emptiedAt, scheduler.DefaultGrace)
	}
	// The return must TRACK the group's death, not merely beat the grace. The
	// bound is one poll interval plus generous scheduling slack, so it stays
	// far below the grace it is distinguishing itself from.
	if slack := elapsed - emptiedAt; slack > 2*time.Second {
		t.Errorf("stopUncommittedRun returned %v after the group emptied; the poll re-probes every %v, so the return must follow the group's death closely", slack, runGroupPollInterval)
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
	// context.Background() (not t.Context()): the child is reaped in t.Cleanup, which runs after t.Context() would already have cancelled it.
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
//
// Unlike its stopUncommittedRun siblings this fixture keeps the survivor as a
// DESCENDANT of the run child rather than a test-owned group member (see
// startTermIgnoringGroupMember): the instantaneous "already gone" assertion is
// what catches a submit-and-return sweep, and a test-owned member would have to
// be reaped by a goroutine here, whose scheduling cannot be ordered against the
// sweep's return. The cost is that this one test needs an ambient reaper for
// the orphaned descendant's zombie, so it passes under CI's init but not in a
// container whose PID 1 never reaps.
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

	got := runRenovateOnce(t.Context(), t.Context().Err, 500*time.Millisecond, "test", runPayload{}, runner)
	if got != runFailed {
		t.Errorf("runRenovateOnce() = %v for a run that exceeded the timeout whose group the sweep confirmed dead, want runFailed", got)
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

// TestWithDumbInitInGroup pins the override's POSITION at the slice level:
// every forwarded entry survives in order and DUMB_INIT_SETSID=0 is appended
// last, which is what makes os/exec's duplicate-key dedup (last value wins)
// deliver exactly one entry to the child. A regression that appended the
// override anywhere but last, or dropped a forwarded entry, goes red here.
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
			name: "a pre-existing entry is outranked by the appended override",
			env:  []string{"DUMB_INIT_SETSID=1", "PATH=/usr/bin"},
			want: []string{"DUMB_INIT_SETSID=1", "PATH=/usr/bin", "DUMB_INIT_SETSID=0"},
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
				t.Errorf("withDumbInitInGroup(%v) = %v, want %v (os/exec keeps the LAST value for a duplicate key, so the override must be appended last)", tt.env, got, tt.want)
			}
		})
	}
}

// TestWithDumbInitInGroup_LeavesTheCallersSliceAlone pins the copy: env is the
// job payload's own slice and the result path still reads it, so appending the
// override must not write into the caller's backing array. The fixture gives
// the input spare capacity and watches the first spare cell, which is the only
// place a bare append could land.
func TestWithDumbInitInGroup_LeavesTheCallersSliceAlone(t *testing.T) {
	t.Parallel()
	env := append(make([]string, 0, 4), "PATH=/usr/bin", "RENOVATE_X=y")
	backing := env[:cap(env)]
	before := slices.Clone(backing)

	got := withDumbInitInGroup(env)

	if !slices.Equal(backing, before) {
		t.Errorf("withDumbInitInGroup() wrote into the caller's backing array: %q, want %q", backing, before)
	}
	if len(got) != len(env)+1 {
		t.Errorf("withDumbInitInGroup() = %v, want the %d forwarded entries plus the override", got, len(env))
	}
}

// TestSweepRunGroupOrWarn_UnconfirmableGroupDeathReportsSurvived drives the
// survived branch of sweepRunGroupOrWarn through the REAL function: the
// group leader is SIGKILLed but deliberately NOT reaped (no Wait), so the
// zombie keeps its process group registered and the sweep's group probe
// reports live members for the whole bounded window -- the same observable
// state as a group whose death cannot be confirmed. sweepRunGroupOrWarn
// must report survived=true (the executor's fatal containment signal) and
// log the production survival message at Warn with the pid. Serial: swaps slog.Default.
func TestSweepRunGroupOrWarn_UnconfirmableGroupDeathReportsSurvived(t *testing.T) {
	rec := capture.Default(t)

	// context.Background() (not t.Context()): the child is reaped in t.Cleanup, which runs after t.Context() would already have cancelled it.
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

	survived := sweepRunGroupOrWarn(cmd, "test")

	if !survived {
		t.Error("sweepRunGroupOrWarn() = false for a group the sweep cannot confirm dead, want true (the fatal containment signal must fire)")
	}
	if got := rec.CountLevel(slog.LevelWarn, "renovate run process group survived the kill sweep; halting run admission to prevent an overlapping run"); got != 1 {
		t.Errorf("Warn records matching the survival message = %d, want 1; captured: %v", got, rec.Messages())
	}
}

// TestStopUncommittedRun_WarnsWhenGroupSurvivesGraceExpirySweep pins the
// last stopUncommittedRun contract: when even the grace-expiry SIGKILL sweep
// cannot confirm the group's death, the helper must still RETURN (bounded by
// the two grace windows -- shutdown must never hang on an unconfirmable
// group) and emit the exact survival Warn operators grep for. The
// unconfirmable state is a SIGKILLed-but-unreaped zombie joined into the
// leader's group (the same observable state
// TestSweepRunGroupOrWarn_UnconfirmableGroupDeathReportsSurvived uses): the
// zombie keeps the group registered through both bounded windows. The
// leader itself honors SIGTERM, so its Wait completes and only the group
// probe stays unsatisfied. Serial: swaps slog.Default. Runtime ~10s (two
// DefaultGrace windows; the constants are library-owned and not injectable).
func TestStopUncommittedRun_WarnsWhenGroupSurvivesGraceExpirySweep(t *testing.T) {
	rec := capture.Default(t)

	readyPath := t.TempDir() + "/ready"
	// context.Background() (not t.Context()): the child is reaped in t.Cleanup, which runs after t.Context() would already have cancelled it.
	cmd := defaultCommandRunner(context.Background(), "sh", "-c", `trap 'exit 0' TERM; : > "$1"; sleep 30 & wait`, "sh", readyPath)
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
	}, "leader did not install its TERM trap")

	// A holder joins the leader's process group, is SIGKILLed but NOT
	// reaped: the zombie keeps the group registered, so neither the grace
	// wait nor the expiry sweep can confirm the group's death.
	holder := exec.Command("sleep", "300")
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}
	if err := holder.Start(); err != nil {
		t.Fatalf("start group holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Wait() }) // reap the zombie after the assertions
	_ = holder.Process.Kill()               // dead but unreaped: a zombie group member

	start := time.Now()
	stopUncommittedRun(cmd)
	stopped = true
	elapsed := time.Since(start)

	if cmd.ProcessState == nil {
		t.Fatal("leader not reaped: Wait never completed")
	}
	if got := rec.Count("uncommitted run process group survived the grace-expiry kill sweep; shutdown may leave it running"); got != 1 {
		t.Errorf("grace-expiry survival Warn count = %d, want 1; captured: %v", got, rec.Messages())
	}
	if elapsed > 25*time.Second {
		t.Errorf("stopUncommittedRun returned after %v; it must stay bounded by the two grace windows, never hang on an unconfirmable group", elapsed)
	}
}

// TestRunRenovateOnce_NonCleanRunsSweepAndReportContainment drives the two
// unwitnessed links in the NON-CLEAN arms of runRenovateOnce, which the clean-arm
// sibling (TestRunRenovateOnce_CleanRunSweepsLeftoverGroupMember) and the
// helper-level sweep test cannot reach: a genuine non-zero exit must still sweep
// its process group before returning, and a sweep that cannot confirm the group's
// death must report runContained rather than runFailed. Both fixtures keep the
// surviving member a direct child of the TEST binary joined into the run leader's
// group, so the test owns the reap instead of measuring the ambient reaper.
func TestRunRenovateOnce_NonCleanRunsSweepAndReportContainment(t *testing.T) {
	t.Parallel()
	// The leader publishes its own pid (its pgid, via Setpgid) so the test can
	// join a member to that group, then blocks until released and exits 1.
	nonZeroLeader := func(leaderPath, releasePath string) scheduler.CommandRunner {
		return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			cmd := defaultCommandRunner(ctx, "sh", "-c",
				`echo $$ > "$1"; until [ -e "$2" ]; do sleep 0.05; done; exit 1`, "sh", leaderPath, releasePath)
			cmd.Stdout, cmd.Stderr = nil, nil // a group member must not hold the test's stdout pipe
			return cmd
		}
	}
	awaitLeaderPID := func(t *testing.T, leaderPath string) int {
		t.Helper()
		waitFor(t, 5*time.Second, func() bool {
			raw, err := os.ReadFile(leaderPath)
			return err == nil && len(strings.TrimSpace(string(raw))) > 0
		}, "the non-zero run leader never published its pid")
		raw, err := os.ReadFile(leaderPath)
		if err != nil {
			t.Fatalf("read the leader pid file: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatalf("bad leader PID file content %q: %v", raw, err)
		}
		return pid
	}

	t.Run("nonzero exit sweeps a surviving group member", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		leaderPath, releasePath := dir+"/leader.pid", dir+"/release"

		resultCh := make(chan runOutcome, 1)
		go func() {
			resultCh <- runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test",
				runPayload{}, nonZeroLeader(leaderPath, releasePath))
		}()

		member := startTermIgnoringGroupMember(t, awaitLeaderPID(t, leaderPath))
		if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
			t.Fatalf("release the non-zero run leader: %v", err)
		}

		if got := <-resultCh; got != runFailed {
			t.Fatalf("runRenovateOnce() = %v, want runFailed: a non-zero exit whose live group member was swept is a failed run, not a containment", got)
		}
		member.awaitReap(t)
		if got := member.exitSignal(); got != syscall.SIGKILL {
			t.Errorf("non-zero run group member exited on signal %v, want SIGKILL: a failed run must still sweep its process group", got)
		}
	})

	t.Run("unconfirmable group death reports containment", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		leaderPath, releasePath := dir+"/leader.pid", dir+"/release"

		resultCh := make(chan runOutcome, 1)
		go func() {
			resultCh <- runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test",
				runPayload{}, nonZeroLeader(leaderPath, releasePath))
		}()

		// A holder joins the leader's group, is SIGKILLed but NOT reaped: the
		// zombie keeps the group registered, so the sweep's probe reports live
		// members for its whole bounded window -- the same observable state as
		// a group whose death cannot be confirmed.
		holder := exec.Command("sleep", "300")
		holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: awaitLeaderPID(t, leaderPath)}
		if err := holder.Start(); err != nil {
			t.Fatalf("start group holder: %v", err)
		}
		t.Cleanup(func() { _ = holder.Wait() }) // reap the zombie after the assertions
		if got, err := syscall.Getpgid(holder.Process.Pid); err != nil || got != holder.SysProcAttr.Pgid {
			t.Fatalf("group holder pgid = (%d, %v), want (%d, nil)", got, err, holder.SysProcAttr.Pgid)
		}
		if err := holder.Process.Kill(); err != nil {
			t.Fatalf("kill group holder: %v", err)
		}
		if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
			t.Fatalf("release the non-zero run leader: %v", err)
		}

		if got := <-resultCh; got != runContained {
			t.Errorf("runRenovateOnce() = %v, want runContained: a non-zero run whose group death cannot be confirmed must raise the containment signal, not report a plain failure", got)
		}
	})
}

// TestRunRenovateOnce_CleanUnconfirmableGroupDeathReportsContainment pins the
// arm where the two containment inputs disagree: Renovate exited ZERO but the
// kill sweep could not confirm its process group dead. The run must report
// runContained so daemon.execute halts admission, and it must NOT emit the
// "renovate run complete" record the completion-absence alerting watches for --
// the clean-arm sibling
// (TestRunRenovateOnce_CleanRunSweepsLeftoverGroupMember) reaches runComplete
// because its member is promptly reaped, and the helper-level sweep test proves
// only that sweepRunGroupOrWarn reports survival. The unconfirmable state is a
// SIGKILLed-but-unreaped zombie joined into the leader's group, a direct child
// of the TEST binary so the test owns the reap. Serial: swaps slog.Default.
func TestRunRenovateOnce_CleanUnconfirmableGroupDeathReportsContainment(t *testing.T) {
	rec := capture.Default(t)
	dir := t.TempDir()
	leaderPath, releasePath := dir+"/leader.pid", dir+"/release"
	// The leader publishes its own pid (its pgid, via Setpgid) so the test can
	// join a member to that group, then blocks until released and exits zero.
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := defaultCommandRunner(ctx, "sh", "-c",
			`echo $$ > "$1"; until [ -e "$2" ]; do sleep 0.05; done`, "sh", leaderPath, releasePath)
		cmd.Stdout, cmd.Stderr = nil, nil // a group member must not hold the test's stdout pipe
		return cmd
	}

	resultCh := make(chan runOutcome, 1)
	go func() {
		resultCh <- runRenovateOnce(t.Context(), t.Context().Err, time.Minute, "test", runPayload{}, runner)
	}()

	waitFor(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(leaderPath)
		return err == nil && len(strings.TrimSpace(string(raw))) > 0
	}, "the clean run leader never published its pid")
	raw, err := os.ReadFile(leaderPath)
	if err != nil {
		t.Fatalf("read the leader pid file: %v", err)
	}
	leaderPid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad leader PID file content %q: %v", raw, err)
	}

	holder := exec.Command("sleep", "300")
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leaderPid}
	if err := holder.Start(); err != nil {
		t.Fatalf("start group holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Wait() }) // reap the zombie after the assertions
	if got, err := syscall.Getpgid(holder.Process.Pid); err != nil || got != leaderPid {
		t.Fatalf("group holder pgid = (%d, %v), want (%d, nil)", got, err, leaderPid)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("kill group holder: %v", err)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release the clean run leader: %v", err)
	}

	if got := <-resultCh; got != runContained {
		t.Errorf("runRenovateOnce() = %v, want runContained: a zero exit whose group death cannot be confirmed must raise the containment signal, not report a completed run", got)
	}
	if got := rec.CountLevel(slog.LevelInfo, "renovate run complete"); got != 0 {
		t.Errorf("INFO records matching %q = %d, want 0: a contained run must not log the completion line; captured: %v", "renovate run complete", got, rec.Messages())
	}
}
