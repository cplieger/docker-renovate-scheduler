package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/health"
	scheduler "github.com/cplieger/scheduler/v3"
	"github.com/cplieger/scheduler/v3/trigger"
)

// newBareDaemon builds the standard test daemon fixture — temp health
// marker, WithoutCancel run context, one-minute timeout, buffered fatal
// channel — WITHOUT starting the executor: each caller owns its runJobs
// goroutine and shutdown ordering. Returns the marker path for tests that
// assert on the marker file directly.
func newBareDaemon(t *testing.T, ctx context.Context, runner scheduler.CommandRunner) (*daemon, string) {
	t.Helper()
	markerPath := filepath.Join(t.TempDir(), "marker")
	d := &daemon{
		queue:   trigger.NewQueue[runPayload](queueCapacity),
		marker:  health.NewMarker(markerPath),
		newCmd:  runner,
		runCtx:  context.WithoutCancel(ctx),
		timeout: time.Minute,
		fatal:   make(chan error, 1),
	}
	return d, markerPath
}

// maxSunPath is the longest usable unix-socket path: Linux sun_path is
// 108 bytes including the trailing NUL.
const maxSunPath = 107

// testSocketPath returns a unix-socket path short enough for sun_path:
// t.TempDir() embeds the full test name, which overflows the limit under a
// long TMPDIR and fails bind with EINVAL. The helper honors a configured
// TMPDIR when the short random directory it yields still fits sun_path
// (keeping test scratch inside a workspace-scoped root), and falls back to
// /tmp — where the production socket lives — only when TMPDIR is too deep.
func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "drs-sock-")
	if err != nil {
		t.Fatalf("mktemp for socket dir: %v", err)
	}
	path := filepath.Join(dir, "s.sock")
	if len(path) > maxSunPath {
		// TMPDIR is too deep for sun_path; fall back to /tmp.
		_ = os.RemoveAll(dir)
		if dir, err = os.MkdirTemp("/tmp", "drs-sock-"); err != nil {
			t.Fatalf("mktemp for socket dir: %v", err)
		}
		path = filepath.Join(dir, "s.sock")
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return path
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

// gatedRunOnce returns a runOnce seam that signals entry into the run
// callback — the committed-run boundary, past execute's shutdownCtx
// preflight re-check — and then blocks until released, reporting a clean
// drained outcome. gatedRunner's readiness file proves only that the child
// process started, NOT that runRenovateOnce committed past its post-Start
// shutdown handshake, so shutdown-drain tests gated on it raced the
// handshake; entry into this callback is the unambiguous barrier.
func gatedRunOnce(t *testing.T) (runOnce runOnceFunc, awaitEntered, release func()) {
	t.Helper()
	entered := make(chan struct{})
	proceed := make(chan struct{})
	runOnce = func(_, _ context.Context, _ time.Duration, _ string, _, _ []string, _ scheduler.CommandRunner) (ok, cancelled, groupSurvived bool) {
		close(entered)
		<-proceed
		return true, false, false
	}
	awaitEntered = func() {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight run never committed")
		}
	}
	var releaseOnce sync.Once
	return runOnce, awaitEntered, func() { releaseOnce.Do(func() { close(proceed) }) }
}
