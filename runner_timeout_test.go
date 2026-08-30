package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestRunRenovateOnce_DeadlineCrossedDuringSweepStillLogsFailure pins the
// timeout classification against the containment sweep's own duration. The
// run's deadline keeps running while sweepRunGroupOrWarn confirms the process
// group is dead (a bounded window of up to scheduler.DefaultGrace), so a
// leader that exited non-zero WITHIN its deadline must still be recorded as a
// failure when that window crosses the deadline -- reading the deadline after
// the sweep instead relabels a real Renovate failure as a timeout, which sends
// triage after a slow run that never happened.
//
// The fixture owns both instants: the leader exits 23 on release, and a
// SIGKILLed-but-unreaped group member (a direct child of the TEST binary, so
// the test reaps it) holds the sweep's probe unsatisfied until the deadline has
// certainly fired. Serial: swaps slog.Default.
func TestRunRenovateOnce_DeadlineCrossedDuringSweepStillLogsFailure(t *testing.T) {
	rec := capture.Default(t)
	const timeout = 2 * time.Second
	dir := t.TempDir()
	leaderPath, releasePath := dir+"/leader.pid", dir+"/release"
	// The leader publishes its own pid (its pgid, via Setpgid) so the test can
	// join a member to that group, then blocks until released and exits 23.
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := defaultCommandRunner(ctx, "sh", "-c",
			`echo $$ > "$1"; until [ -e "$2" ]; do sleep 0.05; done; exit 23`, "sh", leaderPath, releasePath)
		cmd.Stdout, cmd.Stderr = nil, nil // a group member must not hold the test's stdout pipe
		return cmd
	}

	resultCh := make(chan runOutcome, 1)
	started := time.Now()
	go func() {
		resultCh <- runRenovateOnce(t.Context(), t.Context().Err, timeout, "test", runPayload{}, runner)
	}()
	// runRenovateOnce arms its own deadline microseconds after this, so any
	// instant before deadline is certainly inside the run's real window.
	deadline := started.Add(timeout)

	waitFor(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(leaderPath)
		return err == nil && len(strings.TrimSpace(string(raw))) > 0
	}, "the run leader never published its pid")
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
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	// Guard against a vacuous fixture: a holder that failed to join the group
	// leaves the group empty at leader exit, and the sweep then returns before
	// the deadline whatever the code under test does.
	if got, err := syscall.Getpgid(holder.Process.Pid); err != nil || got != leaderPid {
		t.Fatalf("group holder pgid = (%d, %v), want (%d, nil)", got, err, leaderPid)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("kill group holder: %v", err)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release the run leader: %v", err)
	}

	// The leader's pid vanishes when runRenovateOnce's own Wait reaps it,
	// which is the instant the sweep starts.
	waitFor(t, 5*time.Second, func() bool {
		return errors.Is(syscall.Kill(leaderPid, 0), syscall.ESRCH)
	}, "the run leader was never reaped")
	if late := time.Since(deadline); late >= 0 {
		t.Fatalf("invalid fixture: the leader was reaped %v after the run deadline, so this run really did time out; it must exit inside the deadline for the assertions below to mean anything", late)
	}

	// Hold the sweep open past the deadline, then reap the zombie so the
	// probe finds the group empty and the run reports its own exit.
	time.Sleep(time.Until(deadline) + 200*time.Millisecond)
	_ = holder.Wait()

	if got := <-resultCh; got != runFailed {
		t.Errorf("runRenovateOnce() = %v, want runFailed: a non-zero exit inside the deadline stays a failure however long the sweep runs", got)
	}
	if got := rec.CountLevel(slog.LevelError, "renovate run failed"); got != 1 {
		t.Errorf("ERROR records matching %q = %d, want 1; captured: %v", "renovate run failed", got, rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, "renovate run timed out"); got != 0 {
		t.Errorf("ERROR records matching %q = %d, want 0: the deadline passing during the sweep must not relabel a non-zero exit; captured: %v", "renovate run timed out", got, rec.Messages())
	}
}
