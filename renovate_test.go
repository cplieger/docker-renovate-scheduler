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

// TestRunRenovatePass_SkipsWhenLocked verifies the overlap guard: while the
// lock is held, a pass is a deliberate no-op success and Renovate is never
// invoked.
func TestRunRenovatePass_SkipsWhenLocked(t *testing.T) {
	held, ok, err := tryLock(lockFilePath)
	if err != nil || !ok {
		t.Fatalf("failed to pre-acquire lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(held.unlock)

	ran := false
	if ok := runRenovatePass(context.Background(), time.Minute, "test", nil, recordingRunner("true", &ran)); !ok {
		t.Error("runRenovatePass() = false, want true (skip is a no-op success)")
	}
	if ran {
		t.Error("Renovate was invoked despite the overlap lock being held")
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
