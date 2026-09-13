package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/scheduler/v4/trigger"
	"github.com/cplieger/slogx/capture"
)

func newTestDaemon(t *testing.T, runner scheduler.CommandRunner) (*daemon, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d, _ := newBareDaemon(t, runner)
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
	d, markerPath := newBareDaemon(t, recordingRunner("true", nil))
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

func TestExecutor_CarriesRunFailureReason(t *testing.T) {
	tests := []struct {
		name       string
		outcome    runOutcome
		wantReason string
	}{
		{name: "nonzero_exit", outcome: runFailed, wantReason: ""},
		{name: "start_failure", outcome: runStartFailed, wantReason: "failed: renovate could not be started (see the container log stream)"},
		{name: "timeout", outcome: runTimedOut, wantReason: "failed: run exceeded RUN_TIMEOUT (1h0m0s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			d, _ := newBareDaemon(t, recordingRunner("true", nil))
			d.timeout = time.Hour
			d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome {
				return tt.outcome
			}

			j := newJob("external", nil, nil)
			d.execute(t.Context(), t.Context().Err, j)
			out := <-j.Result()

			if out.OK {
				t.Errorf("execute() outcome ok = true for %v, want false", tt.outcome)
			}
			if out.Reason != tt.wantReason {
				t.Errorf("execute() outcome reason = %q for %v, want %q", out.Reason, tt.outcome, tt.wantReason)
			}
		})
	}
}

func TestRunPayload_EnvironmentValuesNeverReachDaemonLogs(t *testing.T) {
	const secret = "test-only-daemon-token-4b87d1"
	rec := capture.Default(t)
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("RUN_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	runner, awaitEntered, release := gatedRunner(t)
	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runDaemon(ctx, sock, runner)
	}()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runDaemon did not stop during test cleanup")
		}
	})

	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "daemon did not bind the trigger socket")
	base := t.TempDir()
	payload := runPayload{Env: []string{
		"RENOVATE_TOKEN=" + secret,
		"RENOVATE_BASE_DIR=" + base,
		"PATH=" + os.Getenv("PATH"),
	}}

	_ = rawRequest(t, sock, payload)
	awaitEntered()
	for i := range queueCapacity {
		dec := rawRequest(t, sock, payload)
		if event := nextEvent(t, dec); event.Kind != trigger.EventQueued {
			t.Fatalf("queued request %d first event = %q, want queued", i, event.Kind)
		}
	}
	overflow := nextEvent(t, rawRequest(t, sock, payload))
	if overflow.Kind != trigger.EventDone || overflow.OK {
		t.Fatalf("overflow event = %+v, want an immediate failed done", overflow)
	}

	cancel()
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runDaemon did not stop after cancellation")
	}

	halted, _ := newBareDaemon(t, recordingRunner("true", nil))
	halted.halted = true
	if err := halted.queue.Submit(newJob("external", nil, payload.Env)); err != nil {
		t.Fatalf("submit to the halted daemon: %v", err)
	}
	halted.queue.Close()
	halted.runJobs(t.Context())
	notDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write preflight fixture: %v", err)
	}
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	job := newJob("external", nil, []string{
		"RENOVATE_TOKEN=" + secret,
		"RENOVATE_BASE_DIR=" + notDir,
	})
	d.execute(t.Context(), t.Context().Err, job)
	<-job.Result()

	if key, value, found := leakedRecord(rec, secret); found {
		t.Errorf("daemon leaked a forwarded environment value in %s = %q", key, value)
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
// explicit reason). Entry into the runOnce seam is past execute's shutdown
// preflight check, so cancellation after awaitEntered observes an in-flight
// run.
func TestExecutor_ShutdownCancelsQueuedButFinishesInFlight(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())

	runOnce, awaitEntered, release := gatedRunOnce(t)
	d, cancel, _ := newTestDaemon(t, recordingRunner("true", nil))
	d.runOnce = runOnce
	// Registered after newTestDaemon's cleanup so it runs first (LIFO): a
	// mid-test Fatal must release the gated run, or the fixture's <-done
	// wait blocks forever on the executor parked inside the runOnce seam.
	t.Cleanup(release)

	inflight := newJob("external", nil, nil)
	if err := d.queue.Submit(inflight); err != nil {
		t.Fatalf("submit(inflight) = %v", err)
	}
	awaitEntered() // the run has committed: execution is inside runOnce

	queued := newJob("external", []string{"owner/q"}, nil)
	if err := d.queue.Submit(queued); err != nil {
		t.Fatalf("submit(queued) = %v", err)
	}

	cancel()              // SIGTERM lands mid-run
	d.health.BeginDrain() // runDaemon's immediate unhealthy transition
	d.queue.Close()       // daemon stops admission
	release()             // the in-flight run finishes its pass

	select {
	case out := <-inflight.Result():
		if !out.OK {
			t.Errorf("in-flight run outcome ok=false, want true (it must drain, not be abandoned)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight result not delivered")
	}
	if d.marker.CheckHealthy() {
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
		if out.Duration != 0 {
			t.Errorf("queued job duration = %v, want 0 because the job never ran", out.Duration)
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
	d, _ := newBareDaemon(t, recordingRunner("true", &argsLog))

	j := newJob("external", nil, nil)
	d.execute(context.WithoutCancel(ctx), ctx.Err, j)

	if len(argsLog) != 0 {
		t.Error("Renovate was invoked despite shutdown being signalled before launch")
	}
	select {
	case out := <-j.Result():
		if out.OK {
			t.Error("outcome ok=true, want a cancelled result")
		}
		if out.Reason != shutdownCancelReason {
			t.Errorf("outcome reason = %q, want %q", out.Reason, shutdownCancelReason)
		}
		if out.Duration <= 0 {
			t.Errorf("outcome duration = %v, want elapsed time for a job that reached execution", out.Duration)
		}
	default:
		t.Fatal("no result delivered for the job cancelled at the launch boundary")
	}
}

// TestExecutor_PreflightTimeoutDoesNotClaimBaseDirUnwritable pins what an
// operator reads when the preflight reaches the same deadline error as an
// elapsed probe budget. The hint must not prescribe a writability remedy.
// Serial: swaps slog.Default.
func TestExecutor_PreflightTimeoutDoesNotClaimBaseDirUnwritable(t *testing.T) {
	rec := capture.Default(t)
	runCtx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	j := newJob("external", nil, nil)

	d.execute(runCtx, t.Context().Err, j)
	out := <-j.Result()

	if out.OK {
		t.Error("preflight timeout outcome ok=true, want false")
	}
	if !strings.Contains(out.Reason, "preflight") {
		t.Errorf("preflight timeout outcome reason = %q, want it to identify a preflight failure", out.Reason)
	}
	if strings.Contains(out.Reason, "not writable") {
		t.Errorf("preflight timeout outcome reason = %q, must not claim a writability verdict", out.Reason)
	}
	hint, ok := rec.AttrValueExact("base directory preflight failed", "hint")
	if !ok {
		t.Fatal("no \"base directory preflight failed\" record carried a hint attribute")
	}
	if strings.Contains(hint, "mount a writable volume") {
		t.Errorf("preflight timeout hint = %q, must not prescribe mounting a volume: no writability verdict was reached", hint)
	}
	if !strings.Contains(hint, "responding") {
		t.Errorf("preflight timeout hint = %q, want it to point at whether the mount is responding", hint)
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

	ctx, stop := context.WithCancel(t.Context())
	tickerDone := startTicker(ctx, d, 15*time.Millisecond, true, 0)

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

func TestStartTicker_StampBecomingDueBeforeSetupFiresStartup(t *testing.T) {
	const interval = time.Hour
	tests := []struct {
		name        string
		remaining   time.Duration
		wantWait    time.Duration
		wantTrigger string
	}{
		{name: "read_just_before_boundary", remaining: time.Millisecond, wantWait: time.Millisecond, wantTrigger: "interval"},
		{name: "read_at_or_after_boundary", remaining: 0, wantWait: 0, wantTrigger: "startup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d, _ := newBareDaemon(t, nil)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				start := time.Now()

				done := startTicker(ctx, d, interval, true, tt.remaining)

				j := <-d.queue.Jobs()
				if waited := time.Since(start); waited != tt.wantWait {
					t.Errorf("startTicker(remaining=%v) fired the first run after %v, want %v", tt.remaining, waited, tt.wantWait)
				}
				if j.Trigger != tt.wantTrigger {
					t.Errorf("startTicker(remaining=%v) first trigger = %q, want %q", tt.remaining, j.Trigger, tt.wantTrigger)
				}
				j.Finish(trigger.Outcome{OK: true})
				cancel()
				<-done
			})
		})
	}
}

// TestStartTicker_DisabledInExternalMode pins that external mode runs no
// ticker: the returned channel is already closed and nothing is submitted.
func TestStartTicker_DisabledInExternalMode(t *testing.T) {
	t.Parallel()
	d := &daemon{queue: trigger.NewQueue[runPayload](4)}
	done := startTicker(t.Context(), d, time.Millisecond, false, 0)
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
	t.Setenv("RUN_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, recordingRunner("true", nil))
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

// TestRunDaemon_BootFailureUpdatesMarkerOnlyAfterAcquiringSocket pins the
// owner-state boundary: a boot that acquires the socket clears stale health
// before a later failure, while a refused second daemon leaves the live
// daemon's marker untouched. Not parallel: it uses healthMarkerPath.
func TestRunDaemon_BootFailureUpdatesMarkerOnlyAfterAcquiringSocket(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	t.Run("unwritable base dir", func(t *testing.T) {
		// A previous life probed healthy: docker restart preserves /tmp.
		if err := os.WriteFile(healthMarkerPath, nil, 0o600); err != nil {
			t.Fatalf("setup previous life's marker: %v", err)
		}
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		t.Setenv("RENOVATE_BASE_DIR", file)
		sock := filepath.Join(t.TempDir(), "trigger.sock")
		if err := runDaemon(t.Context(), sock, recordingRunner("true", nil)); err == nil {
			t.Fatal("runDaemon() = nil, want error")
		}
		if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("healthy marker survived a failed boot; stat err = %v, want not-exist (a crash-looping boot must never probe healthy)", err)
		}
	})
	t.Run("live_socket_owner", func(t *testing.T) {
		if err := os.WriteFile(healthMarkerPath, nil, 0o600); err != nil {
			t.Fatalf("setup previous life's marker: %v", err)
		}
		t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
		sock := filepath.Join(t.TempDir(), "trigger.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("listen on trigger socket: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		if err := runDaemon(t.Context(), sock, recordingRunner("true", nil)); err == nil {
			t.Fatal("runDaemon() = nil, want error")
		}
		if _, err := os.Stat(healthMarkerPath); err != nil {
			t.Errorf("live daemon's healthy marker was changed by a refused boot; stat err = %v, want present", err)
		}
	})
}

// TestRunDaemon_BuiltinModeStartsUnhealthyThenFlipsHealthy is the built-in
// half of the composition-root integration test (the external half is
// TestRunDaemon_ExternalModeBootsHealthyServesAndShutsDownCleanly): with no
// last-run record surviving in the base dir the startup run is DUE, so
// built-in mode boots UNHEALTHY until that run proves the setup, then flips
// healthy — the documented healthcheck contract. The skip side of the
// conditional startup is pinned by TestRunDaemon_ConditionalStartupRun. The
// runner holds the startup run open so the boot state is observable without
// a race. Not parallel: it uses the package-global healthMarkerPath.
func TestRunDaemon_BuiltinModeStartsUnhealthyThenFlipsHealthy(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("RUN_INTERVAL", "6h") // one startup run; no further tick within the test
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		once.Do(func() { close(entered) })
		<-proceed
		return exec.CommandContext(ctx, "true")
	}

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, runner)
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
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	d.marker.Set(true) // a healthy pre-containment state the halt must clear
	d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome {
		invocations++
		return runContained // the group survived the sweep
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
	if d.marker.CheckHealthy() {
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
// shutdown wins daemon.run's select while a run is still draining, and that
// drained run THEN reports its process group survived the kill sweep,
// daemon.run must still return errContainmentLost (main exits non-zero, so
// the container restart reaps the surviving tree) instead of nil. The test
// composes the daemon itself and drives daemon.run — the orchestration the
// composition root delegates to — because the surviving-group report can only
// be injected at the runOnce seam (the same reason
// TestExecutor_HaltsAdmissionAfterSurvivingGroup uses it) and runDaemon
// deliberately carries no test-only parameter. External mode, because the marker boots HEALTHY
// there: BeginDrain — which runs only after the select resolved — flips
// it unhealthy, giving a deterministic post-select signal.
// Not parallel: it uses the package-global healthMarkerPath.
func TestRunDaemon_LateContainmentLossAfterShutdownReturnsError(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var release sync.Once
	runOnce := func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome {
		close(entered)
		<-proceed
		return runContained // the sweep could not confirm group death — reported after shutdown began
	}

	sock := testSocketPath(t)
	ln, err := trigger.Listen(sock)
	if err != nil {
		t.Fatalf("trigger.Listen(%q) = %v", sock, err)
	}
	marker := health.NewMarker(healthMarkerPath)
	d := &daemon{
		queue:     trigger.NewQueue[runPayload](queueCapacity),
		marker:    marker,
		health:    health.NewLatch(marker),
		verifier:  newBaseDirVerifier(),
		stamp:     scheduler.NewStamp(stampPath()),
		newCmd:    recordingRunner("true", nil),
		runOnce:   runOnce,
		stampPath: stampPath(),
		timeout:   time.Minute,
		fatal:     make(chan error, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = d.run(ctx, ln, sock, time.Hour, false, 0)
	}()
	// A mid-test Fatal must not leak the daemon fixture: cancel, release the
	// gated run, and wait for the daemon before later cleanups remove the
	// shared marker and restore the global logger.
	t.Cleanup(func() {
		cancel()
		release.Do(func() { close(proceed) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon.run did not stop during test cleanup")
		}
	})

	// External mode boots healthy; wait for the marker so the trigger below
	// cannot race run's startup. No socket poll: the test bound the listener
	// itself before starting run, and a connection completed into the listen
	// backlog is served whenever Serve begins.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "daemon did not set the health marker healthy on external-mode boot")

	clientDone := make(chan int, 1)
	go func() { clientDone <- runClient(sock, nil) }()
	select {
	case <-entered: // the triggered run is executing
	case <-done:
		t.Fatalf("daemon.run returned before the triggered run began: %v", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("triggered run did not begin")
	}

	cancel() // ordinary shutdown wins the select while the run drains
	// BeginDrain flips the marker unhealthy (file removed) and runs only
	// after the select resolved, so marker absence proves ctx.Done won (the
	// fatal channel was still empty) and the survival report below exercises
	// the post-drain fold-in, not the select's own fatal branch.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return errors.Is(err, fs.ErrNotExist)
	}, "daemon.run did not begin shutdown after cancellation")
	release.Do(func() { close(proceed) }) // the draining run now reports the surviving group

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon.run did not return after the drained run reported containment loss")
	}
	if !errors.Is(runErr, errContainmentLost) {
		t.Errorf("daemon.run() = %v, want errContainmentLost (a late containment loss must still exit non-zero)", runErr)
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
// primary containment-halt path of the orchestration: a run whose process
// group survives the kill sweep during NORMAL operation (no SIGTERM anywhere)
// must shut the daemon down on its own — the fatal branch of daemon.run's
// select — and return errContainmentLost so main exits non-zero and the
// container restart reaps the surviving tree. The executor half is pinned by
// TestExecutor_HaltsAdmissionAfterSurvivingGroup and the post-shutdown
// fold-in by TestRunDaemon_LateContainmentLossAfterShutdownReturnsError; this
// covers the remaining self-initiated shutdown path. The test composes the
// daemon and drives daemon.run directly: the surviving-group report can only
// be injected at the runOnce seam (a SIGKILL-surviving process group cannot
// be fabricated from real test children), and runDaemon deliberately carries
// no test-only parameter. Not parallel: it uses the package-global
// healthMarkerPath.
func TestRunDaemon_ContainmentLossWhileRunningShutsDownWithError(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	runOnce := func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome {
		return runContained // the group survived the sweep mid-operation
	}

	sock := testSocketPath(t)
	ln, err := trigger.Listen(sock)
	if err != nil {
		t.Fatalf("trigger.Listen(%q) = %v", sock, err)
	}
	marker := health.NewMarker(healthMarkerPath)
	d := &daemon{
		queue:     trigger.NewQueue[runPayload](queueCapacity),
		marker:    marker,
		health:    health.NewLatch(marker),
		verifier:  newBaseDirVerifier(),
		stamp:     scheduler.NewStamp(stampPath()),
		newCmd:    recordingRunner("true", nil),
		runOnce:   runOnce,
		stampPath: stampPath(),
		timeout:   time.Minute,
		fatal:     make(chan error, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = d.run(ctx, ln, sock, time.Hour, false, 0)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon.run did not stop during test cleanup")
		}
	})

	// No socket poll: the test bound the listener itself before starting run,
	// so the client's connection lands in the listen backlog and is served
	// whenever Serve begins.
	if code := runClient(sock, nil); code != 1 {
		t.Errorf("runClient() = %d, want 1 (the containment-lost run's trigger must report failure)", code)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon.run did not shut itself down after the containment loss")
	}
	if !errors.Is(runErr, errContainmentLost) {
		t.Errorf("daemon.run() = %v, want errContainmentLost (a mid-operation containment loss must exit non-zero without an external stop)", runErr)
	}
}
