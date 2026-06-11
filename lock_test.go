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
