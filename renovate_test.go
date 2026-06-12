package main

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestRenovateInvocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		repoArgs []string
		wantArgs []string
	}{
		{"no repos runs configured set", nil, []string{"renovate"}},
		{"single repo passed through", []string{"cplieger/homelab"}, []string{"renovate", "cplieger/homelab"}},
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

// recordingRunner returns a commandRunner that records whether it was called
// and runs the fixed binary bin instead of the real entrypoint (which is not
// present in the test environment). /usr/bin/true and /usr/bin/false give
// deterministic exit codes.
func recordingRunner(bin string, ran *bool) commandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		if ran != nil {
			*ran = true
		}
		return exec.CommandContext(ctx, bin)
	}
}

func TestRunRenovatePass_Success(t *testing.T) {
	ran := false
	ok := runRenovatePass(context.Background(), time.Minute, "test", nil, recordingRunner("true", &ran))
	if !ok {
		t.Error("runRenovatePass() = false, want true on a successful run")
	}
	if !ran {
		t.Error("runner was not invoked")
	}
}

func TestRunRenovatePass_Failure(t *testing.T) {
	ok := runRenovatePass(context.Background(), time.Minute, "test", nil, recordingRunner("false", nil))
	if ok {
		t.Error("runRenovatePass() = true, want false when Renovate exits non-zero")
	}
}

// TestRunRenovatePass_QueuesRerunWhenLocked verifies the overlap guard with
// coalescing: while the lock is held, a pass is a deliberate no-op success,
// Renovate is never invoked, and a single rerun is queued for the holder.
func TestRunRenovatePass_QueuesRerunWhenLocked(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	clearRerunPending(rerunFlagPath)

	held, ok, err := tryLock(lockFilePath)
	if err != nil || !ok {
		t.Fatalf("failed to pre-acquire lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(held.unlock)

	ran := false
	if ok := runRenovatePass(context.Background(), time.Minute, "test", nil, recordingRunner("true", &ran)); !ok {
		t.Error("runRenovatePass() = false, want true (overlap is a no-op success)")
	}
	if ran {
		t.Error("Renovate was invoked despite the overlap lock being held")
	}
	if !rerunPending(rerunFlagPath) {
		t.Error("overlapping trigger did not queue a rerun")
	}
}

// flagMarkingRunner returns a commandRunner that records its invocation count
// and, on the first markCount invocations, sets the rerun flag during the
// pass (simulating a trigger arriving mid-run). bin selects the exit code
// ("true"/"false").
func flagMarkingRunner(bin string, calls *int, markCount int) commandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		*calls++
		if *calls <= markCount {
			markRerunPending(rerunFlagPath)
		}
		return exec.CommandContext(ctx, bin)
	}
}

// TestRunRenovatePass_CoalescedRerun verifies that a trigger arriving during a
// pass causes exactly one rerun on completion. The runner marks the flag only
// during the first pass, so the loop runs twice (initial + one rerun) then
// stops — multiple overlapping triggers collapse into a single rerun.
func TestRunRenovatePass_CoalescedRerun(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	clearRerunPending(rerunFlagPath)

	calls := 0
	if ok := runRenovatePass(context.Background(), time.Minute, "test", nil, flagMarkingRunner("true", &calls, 1)); !ok {
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
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	clearRerunPending(rerunFlagPath)

	calls := 0
	// markCount exceeds the cap: every pass re-queues, so only the cap stops it.
	if ok := runRenovatePass(context.Background(), time.Minute, "test", nil, flagMarkingRunner("true", &calls, maxCoalescedReruns+5)); !ok {
		t.Error("runRenovatePass() = false, want true")
	}
	if want := maxCoalescedReruns + 1; calls != want {
		t.Errorf("Renovate invoked %d times, want %d (initial pass + %d capped reruns)", calls, want, maxCoalescedReruns)
	}
}

// TestRunRenovatePass_NoRerunOnFailure verifies a failed pass stops the loop
// even when a rerun was queued, so a failing Renovate isn't hammered.
func TestRunRenovatePass_NoRerunOnFailure(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	clearRerunPending(rerunFlagPath)

	calls := 0
	if ok := runRenovatePass(context.Background(), time.Minute, "test", nil, flagMarkingRunner("false", &calls, 5)); ok {
		t.Error("runRenovatePass() = true, want false when Renovate exits non-zero")
	}
	if calls != 1 {
		t.Errorf("Renovate invoked %d times, want 1 (failure must not rerun despite a queued trigger)", calls)
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
