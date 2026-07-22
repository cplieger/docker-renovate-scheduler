package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	scheduler "github.com/cplieger/scheduler/v2"
)

// testSocketPath returns a unix-socket path short enough for sun_path
// (108 bytes) regardless of TMPDIR: t.TempDir() embeds the full test name,
// which overflows the limit under a long TMPDIR and fails bind with EINVAL.
// /tmp matches where the production socket lives.
func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "drs-sock-")
	if err != nil {
		t.Fatalf("mktemp for socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// shellAssertRunner returns a CommandRunner whose child is a shell running
// script — exit 0 iff the assertion holds — used to verify the child's real
// environment.
func shellAssertRunner(script string) scheduler.CommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

// gatedRunner returns a CommandRunner whose child signals readiness by
// creating a marker file and then blocks until released (exiting 0, so a
// drained run reports its real clean outcome). awaitEntered fails the test
// if the child never starts; release lets it finish its pass.
func gatedRunner(t *testing.T) (runner scheduler.CommandRunner, awaitEntered, release func()) {
	t.Helper()
	dir := t.TempDir()
	enteredPath := filepath.Join(dir, "entered")
	proceedPath := filepath.Join(dir, "proceed")
	runner = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			`: > "$1"; until [ -e "$2" ]; do sleep 0.05; done`, "sh", enteredPath, proceedPath)
	}
	awaitEntered = func() {
		waitFor(t, 5*time.Second, func() bool {
			_, err := os.Stat(enteredPath)
			return err == nil
		}, "in-flight child never started")
	}
	release = func() {
		if err := os.WriteFile(proceedPath, nil, 0o600); err != nil {
			t.Fatalf("release the in-flight child: %v", err)
		}
	}
	return runner, awaitEntered, release
}
