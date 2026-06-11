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
// a Komodo-triggered exec or the built-in ticker). flock associates the
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
