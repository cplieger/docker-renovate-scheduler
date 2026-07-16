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

	scheduler "github.com/cplieger/scheduler/v2"
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
			runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", tt.script)
			}
			if ok := runRenovateOnce(context.Background(), time.Minute, "test", nil, tt.env, runner); !ok {
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
}

// TestRunRenovateOnce_TimeoutCancelsRun pins the documented guardrail that a
// wedged run is killed rather than left running into the next request.
func TestRunRenovateOnce_TimeoutCancelsRun(t *testing.T) {
	t.Parallel()
	slowRunner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "5")
	}

	start := time.Now()
	ok := runRenovateOnce(context.Background(), 100*time.Millisecond, "test", nil, nil, slowRunner)
	elapsed := time.Since(start)

	if ok {
		t.Error("runRenovateOnce() = true for a run that exceeded the timeout, want false")
	}
	if elapsed > 3*time.Second {
		t.Errorf("runRenovateOnce() returned after %v; the timeout did not cancel the run", elapsed)
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
			ok := runRenovateOnce(context.Background(), tt.timeout, "test", nil, nil, tt.runner)
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
	t.Parallel()
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
