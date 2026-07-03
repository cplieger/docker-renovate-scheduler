package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/health"
)

// TestWaitForRunToDrain_NoRunReturnsImmediately verifies the fast path: with
// no run holding the lock, the drain returns true without arming the wait.
func TestWaitForRunToDrain_NoRunReturnsImmediately(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "run.lock")

	start := time.Now()
	drained := waitForRunToDrain(path, 10*time.Millisecond, time.Second)
	elapsed := time.Since(start)

	if !drained {
		t.Error("waitForRunToDrain() = false with no in-flight run, want true")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("waitForRunToDrain() took %v with no in-flight run, want near-immediate", elapsed)
	}
}

// TestWaitForRunToDrain_WaitsForReleaseThenSucceeds verifies the drain blocks
// while a run holds the lock and returns true once the run releases it
// mid-wait — the redeploy-during-run case the drain exists to handle.
func TestWaitForRunToDrain_WaitsForReleaseThenSucceeds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "run.lock")

	held, ok, err := tryLock(path)
	if err != nil || !ok {
		t.Fatalf("failed to acquire the in-flight lock: ok=%v err=%v", ok, err)
	}
	// Release the lock partway through the drain, simulating the in-flight run
	// finishing while the daemon waits. maxWait is far larger than the release
	// delay so the drain succeeds on release, not on timeout.
	go func() {
		time.Sleep(40 * time.Millisecond)
		held.unlock()
	}()

	drained := waitForRunToDrain(path, 10*time.Millisecond, 5*time.Second)

	if !drained {
		t.Error("waitForRunToDrain() = false after the run released the lock, want true")
	}
}

// TestWaitForRunToDrain_TimesOutWhileHeld verifies the drain gives up and
// returns false when the in-flight run does not finish within maxWait (the
// caller then exits and the container stop terminates the lingering run).
func TestWaitForRunToDrain_TimesOutWhileHeld(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "run.lock")

	held, ok, err := tryLock(path)
	if err != nil || !ok {
		t.Fatalf("failed to acquire the in-flight lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(held.unlock)

	start := time.Now()
	drained := waitForRunToDrain(path, 10*time.Millisecond, 60*time.Millisecond)
	elapsed := time.Since(start)

	if drained {
		t.Error("waitForRunToDrain() = true while the lock stayed held, want false")
	}
	if elapsed < 60*time.Millisecond {
		t.Errorf("waitForRunToDrain() returned after %v, want >= maxWait (60ms) — it must wait the full window", elapsed)
	}
}

// TestWaitForRunToDrain_ProbeErrorExitsWithoutDraining covers the probe-error
// fast exit: when runInFlight cannot open the lock (unopenable path, here a
// missing parent dir), the drain logs a warning and returns false immediately
// rather than arming the wait — a broken lock must not hang shutdown.
func TestWaitForRunToDrain_ProbeErrorExitsWithoutDraining(t *testing.T) {
	t.Parallel()
	bad := filepath.Join(t.TempDir(), "missing-subdir", "run.lock")
	start := time.Now()
	drained := waitForRunToDrain(bad, 10*time.Millisecond, time.Second)
	elapsed := time.Since(start)
	if drained {
		t.Error("waitForRunToDrain() = true on a probe error, want false")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("waitForRunToDrain() armed the wait despite a probe error (took %v); a probe error must return immediately", elapsed)
	}
}

// TestRunBuiltin_DrainsInFlightRunAfterShutdown verifies the built-in-mode
// half of the "does not abandon an in-flight run" contract: on shutdown the
// ticker loop stops scheduling, but an in-flight run drains to completion
// because runCtx is decoupled from the shutdown signal (context.WithoutCancel).
// A regression that reverts WithoutCancel to ctx would cancel the in-flight run
// mid-pass (the exit-137 bug the drain was added to fix) and fail the ctxErr
// assertion below.
func TestRunBuiltin_DrainsInFlightRunAfterShutdown(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	clearRerunPending(rerunFlagPath)
	started := make(chan struct{})
	proceed := make(chan struct{})
	var ctxErr error
	var once sync.Once
	runner := func(c context.Context, _ string, _ ...string) *exec.Cmd {
		once.Do(func() { close(started) })
		<-proceed
		ctxErr = c.Err()
		return exec.CommandContext(context.Background(), "true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	marker := health.NewMarker(filepath.Join(t.TempDir(), "marker"))
	done := make(chan struct{})
	go func() {
		runBuiltin(ctx, marker, time.Hour, time.Minute, runner)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("runBuiltin returned before the in-flight run drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runBuiltin did not return after the in-flight run finished")
	}
	if ctxErr != nil {
		t.Errorf("in-flight run ctx err = %v, want nil (WithoutCancel must decouple from shutdown)", ctxErr)
	}
}

// TestRunExternal_BootsHealthyThenDrainsOnShutdown pins external mode's
// documented boot-healthy contract (README: external mode "starts healthy —
// idle, nothing has failed") and the shutdown-then-drain sequence. A
// regression that booted external mode unhealthy would make Docker kill an
// idle, correctly-running container, and nothing else would catch it.
func TestRunExternal_BootsHealthyThenDrainsOnShutdown(t *testing.T) {
	// Not parallel: probes the package-global lockFilePath in /tmp.
	markerPath := filepath.Join(t.TempDir(), "marker")
	marker := health.NewMarker(markerPath)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runExternal(ctx, marker, time.Second)
		close(done)
	}()

	// External mode boots healthy: poll until the marker file appears.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runExternal did not set the health marker healthy on boot")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runExternal did not return after shutdown")
	}

	if _, err := os.Stat(markerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after shutdown marker must be removed; stat err = %v, want not-exist", err)
	}
}

// TestRunBuiltin_SkipsStartupRunWhenAlreadyShutDown covers runBuiltin's
// startup-run skip-on-already-shutdown guard (the boot-time-redeploy guard):
// a stop signalled before the startup goroutine is scheduled must NOT launch a
// fresh run, which -- because runCtx is context.WithoutCancel -- would
// otherwise drain for a full SCHED_TIMEOUT and risk the exit-137 SIGKILL the
// drain exists to prevent.
func TestRunBuiltin_SkipsStartupRunWhenAlreadyShutDown(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	clearRerunPending(rerunFlagPath)

	var ran atomic.Bool
	runner := func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		ran.Store(true)
		return exec.CommandContext(context.Background(), "true")
	}

	// Shutdown signalled before runBuiltin starts (boot-time redeploy).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	marker := health.NewMarker(filepath.Join(t.TempDir(), "marker"))

	done := make(chan struct{})
	go func() {
		runBuiltin(ctx, marker, time.Hour, time.Minute, runner)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runBuiltin did not return promptly with a pre-cancelled context")
	}

	if ran.Load() {
		t.Error("runBuiltin invoked Renovate despite shutdown before startup; must be skipped")
	}
}

// TestRunBuiltin_FiresIntervalRunAfterStartup covers runBuiltin's interval
// ticker arm (the `case <-ticker.C` run), the primary periodic-run behaviour
// of built-in mode. The drain test uses a 1h interval so the ticker never
// fires; a short interval here proves the tick arm runs a pass (startup is the
// 1st invocation, the first tick is the 2nd).
func TestRunBuiltin_FiresIntervalRunAfterStartup(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	clearRerunPending(rerunFlagPath)

	var calls atomic.Int64
	gotTwo := make(chan struct{})
	var once sync.Once
	runner := func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if calls.Add(1) >= 2 {
			once.Do(func() { close(gotTwo) })
		}
		return exec.CommandContext(context.Background(), "true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marker := health.NewMarker(filepath.Join(t.TempDir(), "marker"))

	done := make(chan struct{})
	go func() {
		// Short interval: startup is call 1, first tick is call 2.
		runBuiltin(ctx, marker, 15*time.Millisecond, time.Minute, runner)
		close(done)
	}()

	select {
	case <-gotTwo:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatalf("ticker arm did not fire; runner called %d times (want >= 2)", calls.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runBuiltin did not return after shutdown")
	}
}

// TestRun_ReturnsErrorWhenBaseDirUnwritable pins the daemon composition root's
// fail-fast contract: an unwritable RENOVATE_BASE_DIR makes run return a
// non-nil error (which main turns into a non-zero exit) instead of proceeding
// to schedule runs against a base dir Renovate cannot use. The error is
// returned before any signal handler or health marker is wired, so the check
// is fast and leaves no /tmp state behind.
func TestRun_ReturnsErrorWhenBaseDirUnwritable(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("RENOVATE_BASE_DIR", file)

	if err := run(context.Background()); err == nil {
		t.Error("run() = nil, want a non-nil error when the base dir is unwritable")
	}
}

// TestRunRun_ReturnsExitOneWhenBaseDirUnwritable pins the external `run`
// subcommand's exit-code contract: an unwritable RENOVATE_BASE_DIR must exit
// non-zero (1) so the external trigger (an Ofelia job-exec / external action,
// which treats a non-zero exit as a failed job) surfaces the misconfiguration
// instead of silently reporting success. Returns before the command runner or
// marker is touched, so no real Renovate entrypoint or /tmp marker is involved.
func TestRunRun_ReturnsExitOneWhenBaseDirUnwritable(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("RENOVATE_BASE_DIR", file)

	if code := runRun(context.Background(), nil); code != 1 {
		t.Errorf("runRun() = %d, want 1 when the base dir is unwritable", code)
	}
}
