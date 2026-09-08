package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/scheduler/v4/trigger"
)

// saveLogGlobals captures the three globals slog.SetDefault mutates and restores
// them at test end; call it before the swap. The returned func restores on demand.
//
// SetDefault also aims the log package at the installed handler and skips that
// redirect for slog's own default handler, so reinstalling the previous logger
// cannot undo it; slog's default handler emits through log.Output, so a dead log
// writer silences the package. slog restores first because a non-default previous
// handler re-runs the redirect.
func saveLogGlobals(t *testing.T) func() {
	t.Helper()
	prevLogger, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	restore := func() {
		slog.SetDefault(prevLogger)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}
	t.Cleanup(restore)
	return restore
}

// TestSaveLogGlobals_RestoresTheLogPackageToo red-checks the two restores saveLogGlobals owns
// beyond slog's own; drop either and this test fails.
func TestSaveLogGlobals_RestoresTheLogPackageToo(t *testing.T) {
	prevWriter, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	// Neither the process default nor what SetDefault installs (a slog
	// handlerWriter and 0), so neither assertion can pass by coincidence.
	log.SetOutput(io.Discard)
	log.SetFlags(log.Lshortfile)

	t.Run("swap", func(t *testing.T) {
		_ = saveLogGlobals(t)
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	})

	if got := log.Writer(); got != io.Discard {
		t.Errorf("log.Writer() = %T, want the writer set before the swap: slog.SetDefault aimed log at its own handler and restoring slog alone leaves it there", got)
	}
	if got := log.Flags(); got != log.Lshortfile {
		t.Errorf("log.Flags() = %d, want %d: slog.SetDefault zeroes them and restoring slog alone leaves them at zero", got, log.Lshortfile)
	}
}

func newJob(trig string, repos, env []string) *trigger.Job[runPayload] {
	return trigger.NewJob(trig, runPayload{Repos: repos, Env: env})
}

// The executor is deliberately not started; callers control its lifetime.
func newBareDaemon(t *testing.T, runner scheduler.CommandRunner) (*daemon, string) {
	t.Helper()
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker")
	marker := health.NewMarker(markerPath)
	stampFile := filepath.Join(dir, stampName)
	d := &daemon{
		queue:     trigger.NewQueue[runPayload](queueCapacity),
		marker:    marker,
		health:    health.NewLatch(marker),
		verifier:  newBaseDirVerifier(),
		stamp:     scheduler.NewStamp(stampFile),
		newCmd:    runner,
		stampPath: stampFile,
		timeout:   time.Minute,
		fatal:     make(chan error, 1),
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
	runOnce = func(context.Context, stopRequested, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome {
		close(entered)
		<-proceed
		return runComplete
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

// seedStamp writes a last-run record in the stamp's documented one-line
// format, so tests control the recorded time (Record always stamps now).
func seedStamp(t *testing.T, path string, ts time.Time, outcome string) {
	t.Helper()
	line := ts.UTC().Format(time.RFC3339Nano) + " " + outcome + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("seed stamp file %s: %v", path, err)
	}
}
