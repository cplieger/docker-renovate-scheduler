package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/scheduler/trigger"
	"github.com/cplieger/go/slogx/capture"
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
func submitWait(t *testing.T, d *daemon, j *trigger.Job[runPayload]) trigger.Result {
	t.Helper()
	if err := d.queue.Submit(j); err != nil {
		t.Fatalf("submit() = %v, want nil", err)
	}
	select {
	case out := <-j.Result():
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("job result not delivered within 5s")
		return trigger.Result{}
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

// TestExecutor_RepositoryErrorsFailOnlyAfterTheGrace pins failureGrace: a
// repository error finishes ok with a caveat and leaves health alone until such
// errors have lasted 2h, and a good run ends the series. Full passes and scoped
// runs keep their own series. Serial: swaps slog.Default.
func TestExecutor_RepositoryErrorsFailOnlyAfterTheGrace(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	rec := capture.Default(t)
	d, markerPath := newBareDaemon(t, recordingRunner("true", nil))
	outcome := runComplete
	d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
		return runResult{outcome: outcome}
	}
	run := func(repos ...string) trigger.Result {
		t.Helper()
		j := newJob("external", repos, nil)
		d.execute(t.Context(), t.Context().Done(), j)
		return <-j.Result()
	}

	run()
	// Age the marker, so the tolerated run below must refresh it for the lease.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(markerPath, old, old); err != nil {
		t.Fatal(err)
	}
	outcome = runRepositoryError
	if out := run(); !out.OK || out.Reason != toleratedReason {
		t.Fatalf("first repository error = ok %v reason %q, want ok with %q", out.OK, out.Reason, toleratedReason)
	}
	if !d.marker.Healthy() {
		t.Error("health unhealthy after a tolerated repository error, want it untouched")
	}
	if fi, err := os.Stat(markerPath); err != nil || !fi.ModTime().After(old) {
		t.Errorf("marker not refreshed by a tolerated run (stat err %v), so the built-in lease would lapse in an outage", err)
	}
	if out := run(); !out.OK {
		t.Errorf("second repository error within the grace = ok false (reason %q), want tolerated", out.Reason)
	}

	d.failingSince = d.failingSince.Add(-failureGrace)
	if out := run(); out.OK || out.Reason != escalatedReason {
		t.Fatalf("repository error past the grace = ok %v reason %q, want a failure with %q", out.OK, out.Reason, escalatedReason)
	}
	if d.marker.Healthy() {
		t.Error("health healthy after repository errors lasting the grace, want unhealthy")
	}
	if n := rec.CountLevel(slog.LevelError, ""); n != 1 {
		t.Errorf("ERROR records = %d, want 1 for one failed run; captured: %v", n, rec.Messages())
	}

	outcome = runComplete
	run("owner/scoped")
	if d.failingSince.IsZero() {
		t.Error("a good scoped run ended the full pass's series, want it kept")
	}
	outcome = runRepositoryError
	if out := run(); out.OK {
		t.Error("full-pass repository error past the grace after a good scoped run = ok, want failed")
	}
	if out := run("owner/scoped"); !out.OK {
		t.Error("first scoped repository error = failed, want tolerated on the scoped clock")
	}
	d.scopedFailingSince = d.scopedFailingSince.Add(-failureGrace)
	if out := run("owner/scoped"); out.OK {
		t.Error("scoped repository error past the grace = ok, want failed")
	}

	outcome = runComplete
	run()
	if !d.failingSince.IsZero() || !d.marker.Healthy() {
		t.Errorf("after a good full pass: failingSince = %v, healthy = %v; want zero and healthy", d.failingSince, d.marker.Healthy())
	}
	outcome = runRepositoryError
	if out := run(); !out.OK {
		t.Error("first repository error after a good run = failed, want tolerated")
	}

	outcome = runFailed
	run()
	outcome = runRepositoryError
	if out := run(); !out.OK {
		t.Error("repository error within the grace after a hard failure = failed, want tolerated")
	}
	if d.marker.Healthy() {
		t.Error("a tolerated repository error re-greened health after a hard failure, want it left unhealthy")
	}

	// A tolerated scheduled run still records its real result, so a restart
	// during an outage runs again at boot.
	j := newJob("interval", nil, nil)
	d.execute(t.Context(), t.Context().Done(), j)
	if out := <-j.Result(); !out.OK {
		t.Fatalf("scheduled repository error within the grace = failed (reason %q), want tolerated", out.Reason)
	}
	if stamp, known := d.stamp.Last(); !known || stamp.OK {
		t.Errorf("stamp after a tolerated scheduled run = (%+v, known %v), want a recorded failure", stamp, known)
	}
}

// failureLines drives one run through the executor and returns the lines it
// wrote, parsed.
func failureLines(t *testing.T, d *daemon, read func() []string, from *int) []map[string]string {
	t.Helper()
	j := newJob("external", nil, nil)
	d.execute(t.Context(), t.Context().Done(), j)
	<-j.Result()
	lines := read()
	var out []map[string]string
	for _, l := range lines[*from:] {
		if l != "" {
			out = append(out, parseLogfmt(t, l))
		}
	}
	*from = len(lines)
	return out
}

// withLevel returns the lines at level, only those named msg when msg is set.
func withLevel(lines []map[string]string, level, msg string) []map[string]string {
	var out []map[string]string
	for _, l := range lines {
		if l["level"] == level && (msg == "" || l["msg"] == msg) {
			out = append(out, l)
		}
	}
	return out
}

func failingFor(t *testing.T, line map[string]string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(line["failing_for"])
	if err != nil {
		t.Fatalf("failing_for = %q, not a duration (line %v)", line["failing_for"], line)
	}
	return d
}

// TestExecutor_FirstFailureIsToleratedHoweverLongItsRunLasted pins the first
// failure of a series as tolerated even when the run itself lasted
// failureGrace: the grace counts from the series' first run, which has no
// earlier failure to escalate. Serial: swaps slog.Default.
func TestExecutor_FirstFailureIsToleratedHoweverLongItsRunLasted(t *testing.T) {
	cases := []struct {
		setup func(t *testing.T, d *daemon)
		name  string
		msg   string
	}{
		{name: "repository error", msg: "renovate run ended in a repository error", setup: shellRunner("exit 7")},
		{
			name: "token outage", msg: "renovate run skipped: github app token unavailable",
			setup: tokenServer(http.StatusServiceUnavailable, `{"message":"unavailable"}`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			read := logLines(t)
			d, _ := newBareDaemon(t, recordingRunner("true", nil))
			d.health.Set(true)
			c.setup(t, d)
			lasted := failureGrace + 30*time.Minute
			d.now = func() time.Time { return time.Now().Add(-lasted) }

			j := newJob("external", nil, nil)
			d.execute(t.Context(), t.Context().Done(), j)
			out := <-j.Result()
			var lines []map[string]string
			for _, l := range read() {
				if l != "" {
					lines = append(lines, parseLogfmt(t, l))
				}
			}

			if !out.OK || out.Reason != toleratedReason {
				t.Errorf("outcome = ok %v reason %q, want ok with %q", out.OK, out.Reason, toleratedReason)
			}
			if !d.marker.Healthy() {
				t.Error("health unhealthy after the first failure, want it kept")
			}
			if warns := withLevel(lines, "WARN", c.msg); len(warns) != 1 {
				t.Errorf("wrote WARN lines %v, want one %q", warns, c.msg)
			} else if got := failingFor(t, warns[0]); got < lasted {
				t.Errorf("failing_for = %s, want at least %s", got, lasted)
			}
			if errs := withLevel(lines, "ERROR", ""); len(errs) != 0 {
				t.Errorf("wrote ERROR lines %v, want none", errs)
			}
			records := withLevel(lines, "INFO", runFinishedMsg)
			if len(records) != 1 || records[0]["ok"] != "1" || records[0]["tolerated"] != "1" {
				t.Errorf("run finished records = %v, want one with ok=1 tolerated=1", records)
			}
		})
	}
}

// TestExecutor_OneLinePerFailedRunAndOneOnRecovery pins the log shape of an
// outage: each failed run writes one line, WARN inside failureGrace and ERROR
// past it, carrying how long runs have failed, and the first good run after
// it writes one INFO line saying so. Serial: swaps slog.Default.
func TestExecutor_OneLinePerFailedRunAndOneOnRecovery(t *testing.T) {
	const repoErr = "renovate run ended in a repository error"
	const tokenErr = "renovate run skipped: github app token unavailable"
	const recovered = "renovate runs recovered"
	cases := []struct {
		setup func(t *testing.T, d *daemon)
		heal  func(t *testing.T, d *daemon)
		name  string
		msg   string
	}{
		{name: "repository error", msg: repoErr, setup: shellRunner("exit 7"), heal: shellRunner("exit 0")},
		{
			name: "token outage", msg: tokenErr,
			setup: tokenServer(http.StatusServiceUnavailable, `{"message":"unavailable"}`),
			heal: func(t *testing.T, d *daemon) {
				shellRunner("exit 0")(t, d)
				tokenServer(http.StatusCreated, tokenBody())(t, d)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			read := logLines(t)
			d, _ := newBareDaemon(t, recordingRunner("true", nil))
			c.setup(t, d)
			from := 0

			lines := failureLines(t, d, read, &from)
			if warns := withLevel(lines, "WARN", c.msg); len(warns) != 1 {
				t.Fatalf("failed run inside the grace wrote WARN lines %v, want one %q", warns, c.msg)
			} else if failingFor(t, warns[0]) >= failureGrace {
				t.Errorf("failing_for = %s on the first failure, want under the grace", warns[0]["failing_for"])
			}
			if errs := withLevel(lines, "ERROR", ""); len(errs) != 0 {
				t.Errorf("failed run inside the grace wrote ERROR lines %v, want none", errs)
			}

			d.failingSince = d.failingSince.Add(-failureGrace)
			lines = failureLines(t, d, read, &from)
			errs := withLevel(lines, "ERROR", "")
			if len(errs) != 1 || errs[0]["msg"] != c.msg {
				t.Fatalf("failed run past the grace wrote ERROR lines %v, want exactly one %q", errs, c.msg)
			}
			if failingFor(t, errs[0]) < failureGrace {
				t.Errorf("failing_for = %s past the grace, want at least %s", errs[0]["failing_for"], failureGrace)
			}
			if warns := withLevel(lines, "WARN", c.msg); len(warns) != 0 {
				t.Errorf("failed run past the grace also wrote WARN lines %v, want none", warns)
			}

			c.heal(t, d)
			lines = failureLines(t, d, read, &from)
			var got []map[string]string
			for _, l := range lines {
				if l["msg"] == recovered {
					got = append(got, l)
				}
			}
			if len(got) != 1 || got[0]["level"] != "INFO" {
				t.Fatalf("first good run wrote %v, want one INFO %q", got, recovered)
			}
			if failingFor(t, got[0]) < failureGrace {
				t.Errorf("recovered failing_for = %s, want at least %s", got[0]["failing_for"], failureGrace)
			}

			for _, l := range failureLines(t, d, read, &from) {
				if l["msg"] == recovered {
					t.Errorf("a second good run wrote %v, want no %q", l, recovered)
				}
			}
		})
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
			d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
				return runResult{outcome: tt.outcome}
			}

			j := newJob("external", nil, nil)
			d.execute(t.Context(), t.Context().Done(), j)
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

// TestExecutor_AppModeTimeoutNamesTheLimitThatFired holds a timed-out App-mode
// run's reason to the limit its timeout came from: the token's 30 minutes less
// the 2-minute margin, or a RUN_TIMEOUT that ends sooner. Either way the run
// finished record's result is timed_out. Serial: swaps slog.Default.
func TestExecutor_AppModeTimeoutNamesTheLimitThatFired(t *testing.T) {
	tests := []struct {
		name       string
		runTimeout time.Duration
		wantReason string
	}{
		{name: "the token's life", runTimeout: 2 * time.Hour, wantReason: "failed: run exceeded the GitHub App installation token's life (28m0s)"},
		{name: "RUN_TIMEOUT", runTimeout: 10 * time.Minute, wantReason: "failed: run exceeded RUN_TIMEOUT (10m0s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				read := logLines(t)
				expiry := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
				d := appModeDaemon(t, recordingRunner("true", nil), func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, http.StatusCreated, tokenBodyExpiring(expiry))
				})
				d.timeout = tt.runTimeout
				d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
					return runResult{outcome: runTimedOut, exitCode: noExitCode}
				}

				j := newJob("interval", nil, nil)
				d.execute(t.Context(), t.Context().Done(), j)
				out := <-j.Result()

				if out.OK {
					t.Error("execute() outcome ok = true for a timed-out run, want false")
				}
				if out.Reason != tt.wantReason {
					t.Errorf("execute() outcome reason = %q, want %q", out.Reason, tt.wantReason)
				}
				records := recordsNamed(t, read(), runFinishedMsg)
				if len(records) != 1 || records[0]["result"] != resultTimedOut || records[0]["ok"] != "0" {
					t.Errorf("run finished records = %v, want one with result=%s ok=0", records, resultTimedOut)
				}
			})
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
	d.execute(t.Context(), t.Context().Done(), job)
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
	d.execute(context.WithoutCancel(ctx), ctx.Done(), j)

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

	d.execute(runCtx, t.Context().Done(), j)
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
	d := &daemon{queue: trigger.NewQueue[runPayload](0), metrics: testMetrics(health.NewMarker(filepath.Join(t.TempDir(), "marker")))} // zero capacity: every submit is rejected
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
				j.Finish(trigger.Result{OK: true})
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

// TestRunDaemon_BuiltinModeStartsUnhealthyThenFlipsHealthy: with no last-run
// record in the base dir the startup run is due, so built-in mode boots
// unhealthy until that run proves the setup, then flips healthy. The runner
// holds the startup run open so the boot state is observable without a race.
// Not parallel: it uses the package-global healthMarkerPath.
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

// TestExecutor_HaltsAdmissionAfterSurvivingGroup: once a run reports its
// process group survived the kill sweep, the executor never starts a queued
// run (the survivor may still write the base dir), fails that run and every
// queued waiter with the containment reason, leaves health unhealthy and
// delivers the fatal error. The report is injected at the runOnce seam: a
// SIGKILL-surviving group cannot be fabricated from real test children.
func TestExecutor_HaltsAdmissionAfterSurvivingGroup(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	invocations := 0
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	d.marker.Set(true) // a healthy pre-containment state the halt must clear
	d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
		invocations++
		return runResult{outcome: runContained} // the group survived the sweep
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

// TestRunDaemon_LateContainmentLossAfterShutdownReturnsError: when shutdown
// wins daemon.run's select while a run drains, and that run then reports its
// process group survived the kill sweep, daemon.run still returns
// errContainmentLost, so the container restart reaps the tree. It drives
// daemon.run because the report can only be injected at the runOnce seam. In
// external mode the marker boots healthy, so BeginDrain flipping it unhealthy
// is a deterministic post-select signal. Not parallel: it uses the
// package-global healthMarkerPath.
func TestRunDaemon_LateContainmentLossAfterShutdownReturnsError(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var release sync.Once
	runOnce := func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
		close(entered)
		<-proceed
		return runResult{outcome: runContained} // the sweep could not confirm group death, reported after shutdown began
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
		completed: scheduler.NewStamp(completedStampPath()),
		metrics:   testMetrics(marker),
		newCmd:    recordingRunner("true", nil),
		runOnce:   runOnce,
		now:       time.Now,
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

// TestRunDaemon_ContainmentLossWhileRunningShutsDownWithError: a run whose
// process group survives the kill sweep with no SIGTERM anywhere shuts the
// daemon down through daemon.run's fatal branch and returns
// errContainmentLost, so main exits non-zero and the container restart reaps
// the tree. It drives daemon.run because a SIGKILL-surviving group can only be
// injected at the runOnce seam. Not parallel: it uses the package-global
// healthMarkerPath.
func TestRunDaemon_ContainmentLossWhileRunningShutsDownWithError(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })

	runOnce := func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
		return runResult{outcome: runContained} // the group survived the sweep mid-operation
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
		completed: scheduler.NewStamp(completedStampPath()),
		metrics:   testMetrics(marker),
		newCmd:    recordingRunner("true", nil),
		runOnce:   runOnce,
		now:       time.Now,
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
