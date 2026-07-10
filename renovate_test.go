package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/scheduler"
)

func TestRenovateInvocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		repoArgs []string
		wantArgs []string
	}{
		{"no repos runs configured set", nil, []string{"renovate"}},
		{"single repo passed through", []string{"owner/repo"}, []string{"renovate", "owner/repo"}},
		{"multiple repos passed through", []string{"a/b", "c/d"}, []string{"renovate", "a/b", "c/d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotName, gotArgs := renovateInvocation(tt.repoArgs)
			if gotName != renovateEntrypoint {
				t.Errorf("name = %q, want %q (must route through the image entrypoint)", gotName, renovateEntrypoint)
			}
			if !slices.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}

// recordingRunner returns a scheduler.CommandRunner that records whether it was called
// and runs the fixed binary bin instead of the real entrypoint (which is not
// present in the test environment). /usr/bin/true and /usr/bin/false give
// deterministic exit codes.
func recordingRunner(bin string, ran *bool) scheduler.CommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		if ran != nil {
			*ran = true
		}
		return exec.CommandContext(ctx, bin)
	}
}

// clearRunState resets the package-global /tmp coordination files that tests
// share -- the rerun flag and the drain latch -- and registers their removal on
// cleanup. Both default to absent; a leak from a prior test would otherwise
// make the coalescing loop defer or skip a pass unexpectedly, since the loop's
// pre-pass gate returns early when the drain latch is present.
func clearRunState(t *testing.T) {
	t.Helper()
	rerunFlag.Clear()
	drainFlag.Clear()
	t.Cleanup(func() {
		_ = os.Remove(rerunFlagPath)
		_ = os.Remove(drainMarkerPath)
	})
}

func TestRunRenovatePass_Success(t *testing.T) {
	clearRunState(t)
	ran := false
	ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "test", nil, recordingRunner("true", &ran))
	if !ok {
		t.Error("runRenovatePass() = false, want true on a successful run")
	}
	if !ran {
		t.Error("runner was not invoked")
	}
}

func TestRunRenovatePass_Failure(t *testing.T) {
	clearRunState(t)
	ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "test", nil, recordingRunner("false", nil))
	if ok {
		t.Error("runRenovatePass() = true, want false when Renovate exits non-zero")
	}
}

// TestRunRenovatePass_QueuesRerunWhenLocked verifies the overlap guard with
// coalescing: while the lock is held, a pass is a deliberate no-op success,
// Renovate is never invoked, and a single rerun is queued for the holder.
func TestRunRenovatePass_QueuesRerunWhenLocked(t *testing.T) {
	clearRunState(t)

	held, ok, err := scheduler.TryLock(lockFilePath)
	if err != nil || !ok {
		t.Fatalf("failed to pre-acquire lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(held.Unlock)

	ran := false
	if ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "test", nil, recordingRunner("true", &ran)); !ok {
		t.Error("runRenovatePass() = false, want true (overlap is a no-op success)")
	}
	if ran {
		t.Error("Renovate was invoked despite the overlap lock being held")
	}
	if !rerunFlag.Pending() {
		t.Error("overlapping trigger did not queue a rerun")
	}
}

// flagMarkingRunner returns a scheduler.CommandRunner that records its invocation count
// and, on the first markCount invocations, sets the rerun flag during the
// pass (simulating a trigger arriving mid-run). bin selects the exit code
// ("true"/"false").
func flagMarkingRunner(bin string, calls *int, markCount int) scheduler.CommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		*calls++
		if *calls <= markCount {
			rerunFlag.Set()
		}
		return exec.CommandContext(ctx, bin)
	}
}

// TestRunRenovatePass_CoalescedRerun verifies that a trigger arriving during a
// pass causes exactly one rerun on completion. The runner marks the flag only
// during the first pass, so the loop runs twice (initial + one rerun) then
// stops — multiple overlapping triggers collapse into a single rerun.
func TestRunRenovatePass_CoalescedRerun(t *testing.T) {
	clearRunState(t)

	calls := 0
	if ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "test", nil, flagMarkingRunner("true", &calls, 1)); !ok {
		t.Error("runRenovatePass() = false, want true")
	}
	if calls != 2 {
		t.Errorf("Renovate invoked %d times, want 2 (initial pass + one coalesced rerun)", calls)
	}
}

// TestRunRenovatePass_RerunCapBounded verifies the coalescing loop is bounded:
// when a trigger keeps arriving on every pass, the holder reruns at most
// maxCoalescedReruns times before releasing the lock.
func TestRunRenovatePass_RerunCapBounded(t *testing.T) {
	clearRunState(t)

	calls := 0
	// markCount exceeds the cap: every pass re-queues, so only the cap stops it.
	if ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "test", nil, flagMarkingRunner("true", &calls, maxCoalescedReruns+5)); !ok {
		t.Error("runRenovatePass() = false, want true")
	}
	if want := maxCoalescedReruns + 1; calls != want {
		t.Errorf("Renovate invoked %d times, want %d (initial pass + %d capped reruns)", calls, want, maxCoalescedReruns)
	}
}

// TestRunRenovatePass_NoRerunOnFailure verifies a failed pass stops the loop
// even when a rerun was queued, so a failing Renovate isn't hammered.
func TestRunRenovatePass_NoRerunOnFailure(t *testing.T) {
	clearRunState(t)

	calls := 0
	if ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "test", nil, flagMarkingRunner("false", &calls, 5)); ok {
		t.Error("runRenovatePass() = true, want false when Renovate exits non-zero")
	}
	if calls != 1 {
		t.Errorf("Renovate invoked %d times, want 1 (failure must not rerun despite a queued trigger)", calls)
	}
}

// TestRunRenovatePass_NoRerunAfterShutdown verifies the shutdown gate in the
// coalescing loop: when a trigger queues a rerun during a pass but shutdown is
// signalled before the loop reruns, the holder defers the queued rerun instead
// of launching a fresh pass. In built-in mode runCtx is decoupled from shutdown
// (context.WithoutCancel), so without the gate a rerun would start a fresh
// Renovate run AFTER SIGTERM and drain for another full SCHED_TIMEOUT -- the
// exit-137 SIGKILL the drain feature exists to prevent. shutdownCtx is the
// cancelable argument; runCtx stays uncancelable (context.Background) to mirror
// the built-in context.WithoutCancel wiring.
func TestRunRenovatePass_NoRerunAfterShutdown(t *testing.T) {
	clearRunState(t)

	shutdownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	// One pass during which a trigger arrives (sets the rerun flag) and shutdown
	// is signalled (cancels shutdownCtx). The run executes on runCtx, which is
	// never cancelled, so the pass itself completes; the gate then suppresses
	// the queued rerun.
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		rerunFlag.Set()
		cancel()
		return exec.CommandContext(ctx, "true")
	}
	if ok := runRenovatePass(shutdownCtx, context.Background(), time.Minute, "interval", nil, runner); !ok {
		t.Error("runRenovatePass() = false, want true (deferring a queued rerun on shutdown is a clean outcome)")
	}
	if calls != 1 {
		t.Errorf("Renovate invoked %d times, want 1 (no fresh run may start after shutdown is signalled)", calls)
	}
}

// TestRunRenovatePass_NoRerunWhenDrainLatchSet verifies the external-mode
// shutdown path: when the daemon sets the drain latch during a pass (the only
// way an exec-child `run` learns of the container's shutdown, since docker stop
// signals only PID 1), the coalescing loop lets the in-flight pass finish and
// defers the queued rerun instead of launching a fresh pass that would outlive
// stop_grace_period and be SIGKILLed (exit 137). Mirrors NoRerunAfterShutdown
// but via the cross-process latch, with neither context ever cancelled: an exec
// child never receives the container SIGTERM, so only the latch can stop it.
func TestRunRenovatePass_NoRerunWhenDrainLatchSet(t *testing.T) {
	clearRunState(t)

	calls := 0
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		rerunFlag.Set() // a trigger arrives mid-pass: a rerun is queued
		drainFlag.Set() // the daemon signals shutdown during the pass
		return exec.CommandContext(ctx, "true")
	}
	if ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "external", nil, runner); !ok {
		t.Error("runRenovatePass() = false, want true (draining after a completed pass is a clean outcome)")
	}
	if calls != 1 {
		t.Errorf("Renovate invoked %d times, want 1 (no fresh pass may start once the drain latch is set)", calls)
	}
}

// TestRunRenovatePass_SkipsPassWhenDrainLatchAlreadySet verifies a `run`
// triggered while the daemon is already draining does no work: the loop's
// pre-pass gate sees the latch on the first iteration and returns cleanly
// without invoking Renovate, so a doomed pass is not started during teardown.
func TestRunRenovatePass_SkipsPassWhenDrainLatchAlreadySet(t *testing.T) {
	clearRunState(t)
	drainFlag.Set()

	ran := false
	if ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "external", nil, recordingRunner("true", &ran)); !ok {
		t.Error("runRenovatePass() = false, want true (skipping work during shutdown is a clean outcome)")
	}
	if ran {
		t.Error("Renovate was invoked despite the drain latch being set before the run started")
	}
}

func TestDefaultCommandRunner(t *testing.T) {
	t.Parallel()
	cmd := defaultCommandRunner(context.Background(), "echo", "hi")
	if cmd.Stdout != os.Stdout {
		t.Error("Stdout not wired to os.Stdout (Renovate output must stream through)")
	}
	if cmd.Stderr != os.Stderr {
		t.Error("Stderr not wired to os.Stderr")
	}
	if cmd.WaitDelay != 5*time.Second {
		t.Errorf("WaitDelay = %v, want 5s", cmd.WaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel not set (graceful SIGTERM on shutdown expected)")
	}
}

// TestRunRenovateOnce_TimeoutCancelsRun isolates runRenovateOnce and pins the documented
// guardrail that a wedged run is killed rather than left running into the next tick.
func TestRunRenovateOnce_TimeoutCancelsRun(t *testing.T) {
	slowRunner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "5")
	}

	start := time.Now()
	ok := runRenovateOnce(context.Background(), 100*time.Millisecond, "test", nil, slowRunner)
	elapsed := time.Since(start)

	if ok {
		t.Error("runRenovateOnce() = true for a run that exceeded the timeout, want false")
	}
	if elapsed > 3*time.Second {
		t.Errorf("runRenovateOnce() returned after %v; the timeout did not cancel the run", elapsed)
	}
}

// TestRunRenovateOnce_ShutdownInterruptLogsWarnNotError cancels the PARENT
// context mid-run (the external `run` shutdown path) and pins that the
// interrupt is logged at WARN, not ERROR — a regression to ERROR would trip
// the level=error alert on every graceful shutdown.
func TestRunRenovateOnce_ShutdownInterruptLogsWarnNotError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx, cancel := context.WithCancel(context.Background())
	runner := func(runCtx context.Context, _ string, _ ...string) *exec.Cmd {
		cancel()
		return exec.CommandContext(runCtx, "sleep", "5")
	}
	start := time.Now()
	ok := runRenovateOnce(ctx, time.Minute, "external", nil, runner)
	elapsed := time.Since(start)
	if ok {
		t.Error("runRenovateOnce() = true when interrupted by shutdown, want false")
	}
	if elapsed > 3*time.Second {
		t.Errorf("runRenovateOnce() took %v after a shutdown cancel, want a prompt return", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, "renovate run interrupted by shutdown") {
		t.Errorf("missing the shutdown-interrupt log line; got:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("shutdown interrupt not logged at WARN; got:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("shutdown interrupt wrongly logged at ERROR (would trip level=error alert); got:\n%s", out)
	}
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
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })
			ok := runRenovateOnce(context.Background(), tt.timeout, "test", nil, tt.runner)
			if ok {
				t.Errorf("runRenovateOnce() = true, want false")
			}
			out := buf.String()
			if !strings.Contains(out, tt.wantMsg) {
				t.Errorf("missing %q in log output; got:\n%s", tt.wantMsg, out)
			}
			if !strings.Contains(out, "level=ERROR") {
				t.Errorf("classification not logged at ERROR; got:\n%s", out)
			}
		})
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
// process's stdout pipe open and stall `go test`.
func TestDefaultCommandRunner_CancelSendsSIGTERMNotSIGKILL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := defaultCommandRunner(ctx, "sh", "-c", "trap 'exit 42' TERM; sleep 30 & wait")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the shell install its trap before cancelling
	cancel()

	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait() = %v, want *exec.ExitError from the SIGTERM trap (SIGKILL would not run it)", err)
	}
	if exitErr.ExitCode() != 42 {
		t.Errorf("exit code = %d, want 42: the run must receive SIGTERM (graceful) and run its trap, not SIGKILL", exitErr.ExitCode())
	}
}
