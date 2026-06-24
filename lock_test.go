package main

import (
	"path/filepath"
	"testing"
)

// TestFileLockMutualExclusion verifies the advisory lock contract: the
// first tryLock acquires, a second tryLock on the same path fails while the
// lock is held, and unlock releases it so it can be re-acquired.
func TestFileLockMutualExclusion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "run.lock")

	first, ok, err := tryLock(path)
	if err != nil {
		t.Fatalf("first tryLock: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("first tryLock should acquire the lock")
	}

	if _, ok, err := tryLock(path); err != nil {
		t.Fatalf("second tryLock: unexpected error: %v", err)
	} else if ok {
		t.Error("second tryLock should fail while the lock is held")
	}

	first.unlock()

	again, ok, err := tryLock(path)
	if err != nil {
		t.Fatalf("third tryLock: unexpected error: %v", err)
	}
	if !ok {
		t.Error("tryLock should re-acquire after unlock")
	}
	again.unlock()
}

// TestRerunFlag verifies the coalescing flag primitives: mark sets it, clear
// removes it, rerunPending reflects presence, and both mark and clear are
// idempotent (clearing a missing flag is a no-op).
func TestRerunFlag(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rerun.flag")

	if rerunPending(path) {
		t.Error("rerunPending should be false before the flag is set")
	}
	markRerunPending(path)
	if !rerunPending(path) {
		t.Error("rerunPending should be true after markRerunPending")
	}
	markRerunPending(path) // idempotent: still a single slot
	if !rerunPending(path) {
		t.Error("markRerunPending should be idempotent (still set)")
	}
	clearRerunPending(path)
	if rerunPending(path) {
		t.Error("rerunPending should be false after clearRerunPending")
	}
	clearRerunPending(path) // no-op on a missing flag, must not error or panic
	if rerunPending(path) {
		t.Error("clearRerunPending on a missing flag should leave it clear")
	}
}

// TestRunInFlight reports a held lock as in-flight and a free lock as not, and
// verifies the probe does not retain the lock (the path must be re-acquirable
// after probing). This is the predicate the external-mode shutdown drain polls.
func TestRunInFlight(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "run.lock")

	if inFlight, err := runInFlight(path); err != nil {
		t.Fatalf("runInFlight on a free lock: unexpected error: %v", err)
	} else if inFlight {
		t.Error("runInFlight() = true on a free lock, want false")
	}

	held, ok, err := tryLock(path)
	if err != nil || !ok {
		t.Fatalf("failed to acquire lock for the held case: ok=%v err=%v", ok, err)
	}

	if inFlight, err := runInFlight(path); err != nil {
		t.Fatalf("runInFlight on a held lock: unexpected error: %v", err)
	} else if !inFlight {
		t.Error("runInFlight() = false while the lock is held, want true")
	}

	held.unlock()

	if inFlight, err := runInFlight(path); err != nil || inFlight {
		t.Errorf("runInFlight() after unlock = (%v, %v), want (false, nil) — the probe must not retain the lock", inFlight, err)
	}
}

func TestTryLock_ReturnsErrorWhenPathUnopenable(t *testing.T) {
	t.Parallel()
	bad := filepath.Join(t.TempDir(), "missing-subdir", "run.lock")

	l, ok, err := tryLock(bad)

	if err == nil {
		t.Error("tryLock() error = nil for an unopenable path, want non-nil")
	}
	if ok {
		t.Error("tryLock() ok = true for an unopenable path, want false")
	}
	if l != nil {
		t.Error("tryLock() returned a non-nil lock for an unopenable path, want nil")
	}
}

func TestRunInFlight_PropagatesProbeError(t *testing.T) {
	t.Parallel()
	bad := filepath.Join(t.TempDir(), "missing-subdir", "run.lock")

	inFlight, err := runInFlight(bad)

	if err == nil {
		t.Error("runInFlight() error = nil for an unopenable path, want non-nil")
	}
	if inFlight {
		t.Error("runInFlight() = true on a probe error, want false")
	}
}
