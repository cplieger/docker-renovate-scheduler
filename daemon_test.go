package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	scheduler "github.com/cplieger/scheduler/v2"
	"github.com/cplieger/scheduler/v2/trigger"
	"github.com/cplieger/slogx/capture"
)

// newTestDaemon builds a daemon wired to a temp health marker and the given
// runner, with the executor started. Returns the daemon, the shutdown cancel,
// and a channel closed when the executor has drained.
func newTestDaemon(t *testing.T, runner scheduler.CommandRunner) (*daemon, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d, _ := newBareDaemon(t, ctx, runner)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runJobs(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		d.queue.Close()
		<-done
	})
	return d, cancel, done
}

// submitWait submits a job and returns its outcome.
func submitWait(t *testing.T, d *daemon, j *trigger.Job[runPayload]) trigger.Outcome {
	t.Helper()
	if err := d.queue.Submit(j); err != nil {
		t.Fatalf("submit() = %v, want nil", err)
	}
	select {
	case out := <-j.Result():
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("job result not delivered within 5s")
		return trigger.Outcome{}
	}
}

// TestExecutor_RunsJobsInOrderWithTheirScopes pins the FIFO execution and the
// per-job scope: each run receives ITS OWN repo args — the property the old
// coalescing design lost.
func TestExecutor_RunsJobsInOrderWithTheirScopes(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	var argsLog [][]string
	d, _, _ := newTestDaemon(t, recordingRunner("true", &argsLog))

	a := newJob("external", []string{"owner/a"}, nil)
	b := newJob("external", nil, nil)
	if out := submitWait(t, d, a); !out.OK {
		t.Errorf("job a outcome ok=false, want true")
	}
	if out := submitWait(t, d, b); !out.OK {
		t.Errorf("job b outcome ok=false, want true")
	}
	if len(argsLog) != 2 {
		t.Fatalf("runner invoked %d times, want 2", len(argsLog))
	}
	if want := []string{"renovate", "owner/a"}; !slices.Equal(argsLog[0], want) {
		t.Errorf("run 1 args = %v, want %v (the job's own scope)", argsLog[0], want)
	}
	if want := []string{"renovate"}; !slices.Equal(argsLog[1], want) {
		t.Errorf("run 2 args = %v, want %v (unscoped job runs argless)", argsLog[1], want)
	}
}

// TestExecutor_MarkerFollowsRunOutcome pins the health contract: the marker
// flips healthy on a clean run and unhealthy on a failed one — the executor
// is the marker's single writer.
func TestExecutor_MarkerFollowsRunOutcome(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, markerPath := newBareDaemon(t, ctx, recordingRunner("true", nil))
	done := make(chan struct{})
	go func() { defer close(done); d.runJobs(ctx) }()
	t.Cleanup(func() { cancel(); d.queue.Close(); <-done })

	if out := submitWait(t, d, newJob("external", nil, nil)); !out.OK {
		t.Fatal("clean run reported ok=false")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("marker absent after a clean run: %v (want healthy)", err)
	}

	d.newCmd = recordingRunner("false", nil)
	if out := submitWait(t, d, newJob("external", nil, nil)); out.OK {
		t.Fatal("failed run reported ok=true")
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker present after a failed run; stat err = %v, want not-exist (unhealthy)", err)
	}
}

// TestExecutor_BaseDirFailureFailsRunAndMarker pins the per-run base-dir
// re-verification: a volume that degrades after boot fails the run with an
// actionable reason, flips the marker unhealthy, and never invokes Renovate.
func TestExecutor_BaseDirFailureFailsRunAndMarker(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("RENOVATE_BASE_DIR", file)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var argsLog [][]string
	d, _, _ := newTestDaemon(t, recordingRunner("true", &argsLog))

	out := submitWait(t, d, newJob("external", nil, nil))
	if out.OK {
		t.Error("outcome ok=true with an unwritable base dir, want false")
	}
	if out.Reason == "" {
		t.Error("outcome carries no reason; the client would report a bare failure")
	}
	if len(argsLog) != 0 {
		t.Error("Renovate was invoked despite the base-dir check failing")
	}
}

// TestExecutor_PreflightValidatesForwardedBaseDir pins the per-run preflight
// against the job's FORWARDED environment: a triggered run whose forwarded
// RENOVATE_BASE_DIR is unwritable must fail with an actionable reason and
// never invoke Renovate, even though the daemon's own base dir is writable.
func TestExecutor_PreflightValidatesForwardedBaseDir(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir()) // the daemon's own dir is writable
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var argsLog [][]string
	d, _, _ := newTestDaemon(t, recordingRunner("true", &argsLog))

	j := newJob("external", nil, []string{"RENOVATE_BASE_DIR=" + file, "PATH=" + os.Getenv("PATH")})
	out := submitWait(t, d, j)
	if out.OK {
		t.Error("outcome ok=true with an unwritable forwarded base dir, want false")
	}
	if out.Reason == "" {
		t.Error("outcome carries no reason; the client would report a bare failure")
	}
	if len(argsLog) != 0 {
		t.Error("Renovate was invoked despite the forwarded base-dir preflight failing")
	}
}

// TestExecutor_ShutdownCancelsQueuedButFinishesInFlight pins the drain
// contract: SIGTERM never abandons the in-flight run (it completes with its
// real outcome) and never starts queued work (it is cancelled with an
// explicit reason). The in-flight child pauses AFTER process start — it
// creates a readiness marker, then blocks until released — so the SIGTERM
// lands on a run already committed past runRenovateOnce's post-Start
// shutdown handshake (a pre-Start pause would model the cancelled-start
// window instead, which TestRunRenovateOnce_ShutdownAtStartCancelsAndReapsChild
// covers).
func TestExecutor_ShutdownCancelsQueuedButFinishesInFlight(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())

	runner, awaitEntered, release := gatedRunner(t)
	d, cancel, _ := newTestDaemon(t, runner)

	inflight := newJob("external", nil, nil)
	if err := d.queue.Submit(inflight); err != nil {
		t.Fatalf("submit(inflight) = %v", err)
	}
	awaitEntered() // the run is now executing, post-Start

	queued := newJob("external", []string{"owner/q"}, nil)
	if err := d.queue.Submit(queued); err != nil {
		t.Fatalf("submit(queued) = %v", err)
	}

	cancel()          // SIGTERM lands mid-run
	d.beginShutdown() // runDaemon's immediate unhealthy transition
	d.queue.Close()   // daemon stops admission
	release()         // the in-flight child finishes its pass

	select {
	case out := <-inflight.Result():
		if !out.OK {
			t.Errorf("in-flight run outcome ok=false, want true (it must drain, not be abandoned)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight result not delivered")
	}
	if d.marker.Healthy() {
		t.Error("health marker became healthy after shutdown began (the draining run's completion must not overwrite the shutdown state)")
	}
	select {
	case out := <-queued.Result():
		if out.OK {
			t.Error("queued job outcome ok=true after shutdown, want cancelled")
		}
		if out.Reason == "" {
			t.Error("cancelled job carries no reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued job's cancellation result not delivered")
	}
}

// TestExecutor_ShutdownDuringPreflightNeverStartsRenovate pins the
// child-launch boundary guard: a shutdown that lands after dequeue but
// before the child starts (the base-dir preflight window) cancels the job
// with the explicit shutdown reason and never starts a fresh Renovate pass.
func TestExecutor_ShutdownDuringPreflightNeverStartsRenovate(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown is already signalled when execute reaches the launch boundary

	var argsLog [][]string
	d, _ := newBareDaemon(t, ctx, recordingRunner("true", &argsLog))

	j := newJob("external", nil, nil)
	d.execute(ctx, j)

	if len(argsLog) != 0 {
		t.Error("Renovate was invoked despite shutdown being signalled before launch")
	}
	select {
	case out := <-j.Result():
		if out.OK {
			t.Error("outcome ok=true, want a cancelled result")
		}
		if want := "cancelled: scheduler shutting down"; out.Reason != want {
			t.Errorf("outcome reason = %q, want %q", out.Reason, want)
		}
	default:
		t.Fatal("no result delivered for the job cancelled at the launch boundary")
	}
}

// TestTick_SkipsWhenQueueRejects pins the ticker's degradation: a rejected
// submission (queue full) is logged and skipped — the tick must not panic or
// block; the next interval provides freshness.
func TestTick_SkipsWhenQueueRejects(t *testing.T) {
	d := &daemon{queue: trigger.NewQueue[runPayload](0)} // zero capacity: every submit is rejected
	done := make(chan struct{})
	go func() { defer close(done); d.tick("interval") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick() blocked on a rejected submission; it must skip")
	}
}

// TestStartTicker_FiresStartupThenInterval drives the REAL startTicker and
// pins built-in mode's cadence labels through the daemon's own log lines: the
// first run logs trigger=startup, the next trigger=interval. Not parallel:
// it swaps the global slog default to capture records.
func TestStartTicker_FiresStartupThenInterval(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	rec := capture.Default(t)

	d, cancel, execDone := newTestDaemon(t, recordingRunner("true", nil))

	ctx, stop := context.WithCancel(context.Background())
	tickerDone := startTicker(ctx, d, 15*time.Millisecond, true)

	waitFor(t, 5*time.Second, func() bool {
		return len(startTriggers(rec)) >= 2
	}, "ticker did not fire startup + interval within 5s")
	stop()
	<-tickerDone
	cancel()
	d.queue.Close()
	<-execDone

	triggers := startTriggers(rec)
	if triggers[0] != "startup" {
		t.Errorf("first run trigger = %q, want startup", triggers[0])
	}
	if triggers[1] != "interval" {
		t.Errorf("second run trigger = %q, want interval", triggers[1])
	}
}

// TestStartTicker_DisabledInExternalMode pins that external mode runs no
// ticker: the returned channel is already closed and nothing is submitted.
func TestStartTicker_DisabledInExternalMode(t *testing.T) {
	t.Parallel()
	d := &daemon{queue: trigger.NewQueue[runPayload](4)}
	done := startTicker(context.Background(), d, time.Millisecond, false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startTicker(enabled=false) did not return a closed channel")
	}
	if n := len(d.queue.Jobs()); n != 0 {
		t.Errorf("%d jobs submitted in external mode, want 0", n)
	}
}

// TestRunDaemon_ExternalModeBootsHealthyServesAndShutsDownCleanly is the
// composition-root integration test: external mode boots healthy (idle),
// serves a triggered run over the real socket, and on shutdown removes the
// socket and the marker. Not parallel: it uses the package-global
// healthMarkerPath (the real path the health subcommand probes).
func TestRunDaemon_ExternalModeBootsHealthyServesAndShutsDownCleanly(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("SCHED_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, recordingRunner("true", nil), nil)
	}()

	// External mode boots healthy: poll until the marker appears.
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "daemon did not set the health marker healthy on external-mode boot")
	// The socket must be live and serving.
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "daemon did not bind the trigger socket")

	if code := runClient(sock, []string{"owner/repo"}); code != 0 {
		t.Errorf("runClient() = %d, want 0 (clean triggered run)", code)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return after shutdown")
	}
	if runErr != nil {
		t.Errorf("runDaemon() = %v, want nil", runErr)
	}
	if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker not cleaned up on shutdown; stat err = %v, want not-exist", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket file not removed on shutdown; stat err = %v, want not-exist", err)
	}
}

// waitFor polls cond until true or the deadline, failing the test with msg.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startTriggers returns the trigger= attr of each "renovate run starting"
// record, in capture order — the scheduler is logs-only, so its structured
// lines are the observable output for cadence assertions.
func startTriggers(rec *capture.Recorder) []string {
	var out []string
	for _, r := range rec.Records() {
		if r.Message != "renovate run starting" {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "trigger" {
				out = append(out, a.Value.String())
				return false
			}
			return true
		})
	}
	return out
}

// TestRunDaemon_BootFailuresReturnError pins the fail-fast boot contract:
// an unwritable base dir or an unbindable trigger socket must fail runDaemon
// with an error (main exits non-zero, so the container restarts loudly)
// instead of booting a daemon that cannot run or cannot be triggered.
func TestRunDaemon_BootFailuresReturnError(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Run("unwritable base dir fails boot", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		t.Setenv("RENOVATE_BASE_DIR", file)
		sock := filepath.Join(t.TempDir(), "trigger.sock")
		if err := runDaemon(context.Background(), sock, recordingRunner("true", nil), nil); err == nil {
			t.Error("runDaemon() = nil, want error when the base dir is unwritable at boot")
		}
	})
	t.Run("unbindable socket path fails boot", func(t *testing.T) {
		t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
		sock := filepath.Join(t.TempDir(), "missing-parent", "trigger.sock")
		if err := runDaemon(context.Background(), sock, recordingRunner("true", nil), nil); err == nil {
			t.Error("runDaemon() = nil, want error when the socket cannot be bound")
		}
	})
}

// TestRunDaemon_BuiltinModeStartsUnhealthyThenFlipsHealthy is the built-in
// half of the composition-root integration test (the external half is
// TestRunDaemon_ExternalModeBootsHealthyServesAndShutsDownCleanly): built-in
// mode boots UNHEALTHY until the startup run proves the setup, then flips
// healthy — the documented healthcheck contract. The runner holds the startup
// run open so the boot state is observable without a race. Not parallel: it
// uses the package-global healthMarkerPath.
func TestRunDaemon_BuiltinModeStartsUnhealthyThenFlipsHealthy(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("SCHED_INTERVAL", "6h") // one startup run; no further tick within the test
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		once.Do(func() { close(entered) })
		<-proceed
		return exec.CommandContext(ctx, "true")
	}

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, runner, nil)
	}()

	select {
	case <-entered: // the startup run is executing; the marker must still be unhealthy
	case <-done: // boot failed before the startup run; fail fast with the cause
		t.Fatalf("runDaemon returned before the startup run began: %v", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("startup run did not begin")
	}
	if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker present before the first run completed; stat err = %v, want not-exist (built-in mode boots unhealthy)", err)
	}
	close(proceed) // let the startup run finish

	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "marker not set healthy after the startup run completed")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return after shutdown")
	}
	if runErr != nil {
		t.Errorf("runDaemon() = %v, want nil", runErr)
	}
}

// TestExecutor_HaltsAdmissionAfterSurvivingGroup is the containment-loss
// regression test: when a run reports that its process group survived the
// post-run kill sweep, the executor must never invoke a second queued run
// (the survivor may still be writing the base dir), must fail the surviving
// run and every queued waiter with the explicit containment reason, must
// leave the health marker unhealthy, and must deliver the fatal error
// runDaemon exits non-zero on. The surviving-group report is injected at the
// runOnce seam: a SIGKILL-surviving process group cannot be fabricated from
// real test children.
func TestExecutor_HaltsAdmissionAfterSurvivingGroup(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	invocations := 0
	d, _ := newBareDaemon(t, ctx, recordingRunner("true", nil))
	d.runOnce = func(_, _ context.Context, _ time.Duration, _ string, _, _ []string, _ scheduler.CommandRunner) (ok, cancelled, groupSurvived bool) {
		invocations++
		return false, false, true // the group survived the sweep
	}

	first := newJob("external", nil, nil)
	second := newJob("external", []string{"owner/q"}, nil)
	if err := d.queue.Submit(first); err != nil {
		t.Fatalf("submit(first) = %v", err)
	}
	if err := d.queue.Submit(second); err != nil {
		t.Fatalf("submit(second) = %v", err)
	}

	done := make(chan struct{})
	go func() { defer close(done); d.runJobs(ctx) }()
	t.Cleanup(func() { cancel(); d.queue.Close(); <-done })

	for _, tc := range []struct {
		name string
		j    *trigger.Job[runPayload]
	}{{"surviving run", first}, {"queued waiter", second}} {
		select {
		case out := <-tc.j.Result():
			if out.OK {
				t.Errorf("%s outcome ok=true, want false", tc.name)
			}
			if out.Reason != containmentLostReason {
				t.Errorf("%s outcome reason = %q, want %q", tc.name, out.Reason, containmentLostReason)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s result not delivered", tc.name)
		}
	}
	if invocations != 1 {
		t.Errorf("runOnce invoked %d times, want 1: no run may start after a surviving group", invocations)
	}
	if d.marker.Healthy() {
		t.Error("health marker healthy after containment loss, want unhealthy")
	}
	select {
	case err := <-d.fatal:
		if !errors.Is(err, errContainmentLost) {
			t.Errorf("fatal error = %v, want errContainmentLost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fatal containment error not delivered to runDaemon's channel")
	}
}

// TestRunDaemon_LateContainmentLossAfterShutdownReturnsError is the
// shutdown-ordering half of the containment-loss contract: when ordinary
// shutdown wins runDaemon's select while a run is still draining, and that
// drained run THEN reports its process group survived the kill sweep,
// runDaemon must still return errContainmentLost (main exits non-zero, so
// the container restart reaps the surviving tree) instead of nil. The
// surviving-group report is injected via runDaemon's runOnce parameter for
// the same reason TestExecutor_HaltsAdmissionAfterSurvivingGroup uses the
// daemon.runOnce seam. External mode, because the marker boots HEALTHY
// there: beginShutdown — which runs only after the select resolved — flips
// it unhealthy, giving a deterministic post-select signal (runDaemon's
// setupLogger replaces the slog default, so log capture cannot provide it).
// Not parallel: it uses the package-global healthMarkerPath.
func TestRunDaemon_LateContainmentLossAfterShutdownReturnsError(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("SCHED_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var release sync.Once
	runOnce := func(_, _ context.Context, _ time.Duration, _ string, _, _ []string, _ scheduler.CommandRunner) (ok, cancelled, groupSurvived bool) {
		close(entered)
		<-proceed
		return false, false, true // the sweep could not confirm group death — reported after shutdown began
	}

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, recordingRunner("true", nil), runOnce)
	}()
	// A mid-test Fatal must not leak the daemon fixture: cancel, release the
	// gated run, and wait for runDaemon before later cleanups remove the
	// shared marker and restore the global logger.
	t.Cleanup(func() {
		cancel()
		release.Do(func() { close(proceed) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("runDaemon did not stop during test cleanup")
		}
	})

	// External mode boots healthy and binds the socket; wait for both so the
	// trigger below cannot race the boot.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "daemon did not set the health marker healthy on external-mode boot")
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "daemon did not bind the trigger socket")

	clientDone := make(chan int, 1)
	go func() { clientDone <- runClient(sock, nil) }()
	select {
	case <-entered: // the triggered run is executing
	case <-done:
		t.Fatalf("runDaemon returned before the triggered run began: %v", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("triggered run did not begin")
	}

	cancel() // ordinary shutdown wins the select while the run drains
	// beginShutdown flips the marker unhealthy (file removed) and runs only
	// after the select resolved, so marker absence proves ctx.Done won (the
	// fatal channel was still empty) and the survival report below exercises
	// the post-drain fold-in, not the select's own fatal branch.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return errors.Is(err, fs.ErrNotExist)
	}, "runDaemon did not begin shutdown after cancellation")
	release.Do(func() { close(proceed) }) // the draining run now reports the surviving group

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return after the drained run reported containment loss")
	}
	if !errors.Is(runErr, errContainmentLost) {
		t.Errorf("runDaemon() = %v, want errContainmentLost (a late containment loss must still exit non-zero)", runErr)
	}
	select {
	case code := <-clientDone:
		if code != 1 {
			t.Errorf("runClient() = %d, want 1 (the surviving run's waiter gets the containment failure)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("triggered run's client did not exit")
	}
}

// TestRunDaemon_ContainmentLossWhileRunningShutsDownWithError pins the
// primary containment-halt path at the composition root: a run whose process
// group survives the kill sweep during NORMAL operation (no SIGTERM anywhere)
// must shut the daemon down on its own — the fatal branch of runDaemon's
// select — and return errContainmentLost so main exits non-zero and the
// container restart reaps the surviving tree. The executor half is pinned by
// TestExecutor_HaltsAdmissionAfterSurvivingGroup and the post-shutdown
// fold-in by TestRunDaemon_LateContainmentLossAfterShutdownReturnsError; this
// covers the remaining self-initiated shutdown path. The surviving-group
// report is injected via runDaemon's runOnce parameter for the same reason
// those tests use the seam: a SIGKILL-surviving process group cannot be
// fabricated from real test children. Not parallel: it uses the
// package-global healthMarkerPath.
func TestRunDaemon_ContainmentLossWhileRunningShutsDownWithError(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("SCHED_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	runOnce := func(_, _ context.Context, _ time.Duration, _ string, _, _ []string, _ scheduler.CommandRunner) (ok, cancelled, groupSurvived bool) {
		return false, false, true // the group survived the sweep mid-operation
	}

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, recordingRunner("true", nil), runOnce)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("runDaemon did not stop during test cleanup")
		}
	})

	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "daemon did not bind the trigger socket")

	if code := runClient(sock, nil); code != 1 {
		t.Errorf("runClient() = %d, want 1 (the containment-lost run's trigger must report failure)", code)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not shut itself down after the containment loss")
	}
	if !errors.Is(runErr, errContainmentLost) {
		t.Errorf("runDaemon() = %v, want errContainmentLost (a mid-operation containment loss must exit non-zero without an external stop)", runErr)
	}
}

// TestExecutor_CancelledRunDeliversShutdownReasonAndLeavesMarker pins
// execute's cancelled branch: when runRenovateOnce reports the post-Start
// shutdown handshake reaped the child (cancelled=true), the waiter must get
// the explicit shutdown reason — not a bare failure — and the health marker
// must be left alone (beginShutdown owns the shutdown health state; a
// cancelled start is not a run failure). The cancelled report is injected at
// the runOnce seam; the real handshake behavior is pinned process-level by
// TestRunRenovateOnce_ShutdownAtStartCancelsAndReapsChild.
func TestExecutor_CancelledRunDeliversShutdownReasonAndLeavesMarker(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())

	ctx := t.Context()
	d, _ := newBareDaemon(t, ctx, recordingRunner("true", nil))
	d.marker.Set(true) // the pre-shutdown health state must survive the cancellation
	d.runOnce = func(_, _ context.Context, _ time.Duration, _ string, _, _ []string, _ scheduler.CommandRunner) (ok, cancelled, groupSurvived bool) {
		return false, true, false // the post-Start handshake reaped the child
	}

	j := newJob("external", nil, nil)
	d.execute(ctx, j)

	select {
	case out := <-j.Result():
		if out.OK {
			t.Error("outcome ok=true for a shutdown-cancelled run, want false")
		}
		if out.Reason != shutdownCancelReason {
			t.Errorf("outcome reason = %q, want %q", out.Reason, shutdownCancelReason)
		}
	default:
		t.Fatal("no result delivered for the cancelled run")
	}
	if !d.marker.Healthy() {
		t.Error("health marker flipped unhealthy by a cancelled run; the cancelled branch must leave the marker alone (beginShutdown owns the shutdown state)")
	}
}
