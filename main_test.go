package main

import (
	"path/filepath"
	"testing"
	"time"
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
