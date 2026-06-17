package main

import (
	"os"
	"path/filepath"
	"testing"
)

// gk_docker_renovate_scheduler_u1_countOpenFDs returns the number of open file
// descriptors held by the current process by reading /proc/self/fd. The
// readdir's own transient directory descriptor is counted symmetrically on
// every call, so it cancels out when callers compare two counts. The repo
// targets Linux (distroless containers) and gremlins runs on Linux, so the
// path is always present there; the skip guards a non-Linux developer run.
func gk_docker_renovate_scheduler_u1_countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unreadable, FD-leak detection unavailable: %v", err)
	}
	return len(entries)
}

// TestMarkRerunPending_closesDescriptorOnSuccess kills the CONDITIONALS_NEGATION
// mutant at lock.go:56:70 (err == nil -> err != nil).
//
// os.OpenFile runs unconditionally as the if-init, so the flag file is created
// on disk in BOTH the original and the mutant — every rerunPending-based
// assertion passes for both, which is why this mutant survives the existing
// TestRerunFlag. The only behavioural difference is the success path: the
// original ("if err == nil") calls f.Close(), releasing the descriptor
// synchronously before markRerunPending returns; the mutant ("if err != nil")
// skips the close on every successful open and leaks one descriptor per call.
//
// The original therefore keeps the process FD count flat across many calls
// (each descriptor is opened and closed, and the number is reused), while the
// mutant's count grows by one per call. The loop performs only a handful of
// tiny allocations, far below Go's GC trigger, so no *os.File finalizer runs
// to mask the leak; the growth is deterministic. No runtime.GC() is invoked
// (that would close the mutant's leaked descriptors and weaken the kill), and
// the test is intentionally not parallel so no sibling test churns FDs during
// the measurement window.
func TestMarkRerunPending_closesDescriptorOnSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rerun.flag")

	// Warm up: create the flag file and trip any lazy descriptor
	// initialisation so the baseline below is stable. After this, every
	// subsequent open takes the identical (file-already-exists) code path.
	for range 5 {
		markRerunPending(path)
	}

	const gk_docker_renovate_scheduler_u1_iters = 64
	const gk_docker_renovate_scheduler_u1_maxGrowth = gk_docker_renovate_scheduler_u1_iters / 4 // 16

	before := gk_docker_renovate_scheduler_u1_countOpenFDs(t)
	for range gk_docker_renovate_scheduler_u1_iters {
		markRerunPending(path)
	}
	after := gk_docker_renovate_scheduler_u1_countOpenFDs(t)

	growth := after - before
	if growth >= gk_docker_renovate_scheduler_u1_maxGrowth {
		t.Errorf("markRerunPending(path) leaked descriptors: FD growth over %d calls = %d, want < %d (success path must Close the opened file)",
			gk_docker_renovate_scheduler_u1_iters, growth, gk_docker_renovate_scheduler_u1_maxGrowth)
	}
}
