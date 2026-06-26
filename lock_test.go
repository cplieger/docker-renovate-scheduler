package main

import (
	"os"
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

// countOpenFDs returns the number of open file descriptors held by the current
// process by reading /proc/self/fd. The readdir's own transient directory
// descriptor is counted symmetrically on every call, so it cancels out when
// callers compare two counts. The repo targets Linux (container deployment)
// and CI runs on Linux, so the path is always present there; the skip guards a
// non-Linux developer run.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unreadable, FD-leak detection unavailable: %v", err)
	}
	return len(entries)
}

// TestMarkRerunPending_closesDescriptorOnSuccess verifies markRerunPending
// closes the descriptor it opens on the success path, so repeated coalescing
// triggers do not leak file descriptors.
//
// os.OpenFile runs unconditionally, so the flag file lands on disk whether or
// not the success path closes the descriptor -- every rerunPending-based
// assertion (see TestRerunFlag) passes either way. The only observable
// difference is the descriptor: closing it synchronously before returning keeps
// the process FD count flat across many calls (the descriptor number is
// reused), whereas skipping the close leaks one descriptor per call and the
// count grows by one each time.
//
// The loop performs only a handful of tiny allocations, far below Go's GC
// trigger, so no *os.File finalizer runs to mask a leak; the growth would be
// deterministic. No runtime.GC() is invoked (that would close any leaked
// descriptors and weaken the check), and the test is intentionally not parallel
// so no sibling test churns FDs during the measurement window.
func TestMarkRerunPending_closesDescriptorOnSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rerun.flag")

	// Warm up: create the flag file and trip any lazy descriptor
	// initialisation so the baseline below is stable. After this, every
	// subsequent open takes the identical (file-already-exists) code path.
	for range 5 {
		markRerunPending(path)
	}

	const iters = 64
	const maxGrowth = iters / 4 // 16

	before := countOpenFDs(t)
	for range iters {
		markRerunPending(path)
	}
	after := countOpenFDs(t)

	growth := after - before
	if growth >= maxGrowth {
		t.Errorf("markRerunPending(path) leaked descriptors: FD growth over %d calls = %d, want < %d (success path must Close the opened file)",
			iters, growth, maxGrowth)
	}
}
