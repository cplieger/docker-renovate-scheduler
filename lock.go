package main

import (
	"errors"
	"os"
	"syscall"
)

// --- Run Overlap Lock ---

// fileLock is an advisory exclusive lock backed by flock(2). It is the
// single overlap-prevention mechanism for both scheduling modes: the
// built-in ticker (in-process, startup run vs interval run) and the
// external `run` subcommand (cross-process, a scheduled Ofelia exec racing
// an externally-triggered exec or the built-in ticker). flock associates the
// lock with the open file description, so two independent os.OpenFile calls
// contend even within one process.
type fileLock struct {
	f *os.File
}

// tryLock attempts a non-blocking exclusive lock on path. ok is false when
// another holder currently owns the lock (a run is already in flight). On
// ok the caller must release the lock with unlock.
func tryLock(path string) (l *fileLock, ok bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644) // #nosec G304 -- fixed in-container lock path
	if err != nil {
		return nil, false, err
	}
	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
		_ = f.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, lockErr
	}
	return &fileLock{f: f}, true, nil
}

// unlock releases the lock and closes the underlying file. The lockfile
// itself is left on disk; it carries no state and is reused across runs.
func (l *fileLock) unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// runInFlight reports whether a Renovate run currently holds the overlap lock
// at path — i.e. the built-in ticker or an external `run` exec is mid-pass. It
// probes with a non-blocking lock attempt: a free lock is acquired and
// immediately released (not in flight); a lock another holder owns fails with
// EWOULDBLOCK (in flight). Because flock releases automatically when the
// holding process exits, a crashed run never reports as perpetually in flight.
// The external-mode shutdown drain uses this to wait out an in-flight run
// before the daemon exits, so a redeploy doesn't SIGKILL it mid-pass.
func runInFlight(path string) (inFlight bool, err error) {
	l, ok, err := tryLock(path)
	if err != nil {
		return false, err
	}
	if ok {
		l.unlock()
		return false, nil
	}
	return true, nil
}

// --- Rerun coalescing flag ---

// markRerunPending records, via a single-slot flag file, that a trigger
// arrived while a run held the overlap lock, so the active holder reruns once
// on completion. Idempotent: the flag is boolean, so any number of
// overlapping triggers coalesce into exactly one queued rerun ("max 1 wait").
// Best-effort — a failed write (a broken /tmp would also break the lock and
// the health marker) simply defers the trigger to the next scheduled run.
func markRerunPending(path string) {
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err == nil { // #nosec G304 -- fixed in-container flag path
		_ = f.Close()
	}
}

// rerunPending reports whether a coalesced rerun was queued during the pass
// (the flag file exists).
func rerunPending(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// clearRerunPending removes the flag at the start of each pass so only
// triggers arriving during that pass queue the next rerun (clearing before
// the pass, not after, prevents lost wakeups). Best-effort: a missing flag is
// the desired state, and a stale flag is cleared on the next pass anyway.
func clearRerunPending(path string) {
	_ = os.Remove(path)
}
