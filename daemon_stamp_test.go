package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx/capture"
)

const startupSkippedMsg = "startup run skipped: the last scheduled run succeeded within the interval"

// TestRunDaemon_ConditionalStartupRun pins the boot decision end to end: the
// startup run fires unless a SUCCESSFUL scheduled run completed within
// RUN_INTERVAL, judged from the last-run record in the base dir. A failed or
// stale record fires the run again (an operator who fixes a bad config and
// recreates the container gets immediate feedback); a missing record — no
// persisted volume — degrades to the unconditional startup run. Not
// parallel: it uses the package-global healthMarkerPath and swaps the global
// slog default to capture records.
func TestRunDaemon_ConditionalStartupRun(t *testing.T) {
	tests := []struct {
		name    string
		outcome string // "" = no record seeded
		age     time.Duration
		wantRun bool
	}{
		{"no record fires the startup run", "", 0, true},
		{"fresh success skips the startup run", "ok", time.Hour, false},
		{"fresh failure fires the startup run", "failed", time.Hour, true},
		{"stale success fires the startup run", "ok", 7 * time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("RENOVATE_BASE_DIR", base)
			t.Setenv("RUN_INTERVAL", "6h") // at most the startup run fires within the test
			t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
			seedTime := time.Now().Add(-tt.age).UTC()
			if tt.outcome != "" {
				seedStamp(t, filepath.Join(base, stampName), seedTime, tt.outcome)
			}
			rec := capture.Default(t)

			if tt.wantRun {
				assertStartupRunFires(t, rec)
				return
			}
			assertStartupRunSkipped(t, rec, seedTime)
		})
	}
}

// assertStartupRunFires boots the daemon and requires a due boot: unhealthy
// while the startup run is in flight, healthy after it completes, and the
// run labelled trigger=startup.
func assertStartupRunFires(t *testing.T, rec *capture.Recorder) {
	t.Helper()
	runner, awaitEntered, release := gatedRunner(t)
	cancel, done, runErr := startDaemonForTest(t, runner)

	awaitEntered()
	if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker present while the due startup run is in flight; stat err = %v, want not-exist (a due boot is unhealthy)", err)
	}
	release()
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "marker not set healthy after the startup run completed")

	cancel()
	awaitDaemonStopped(t, done)
	if err := *runErr; err != nil {
		t.Errorf("runDaemon() = %v, want nil", err)
	}
	triggers := startTriggers(rec)
	if len(triggers) == 0 || triggers[0] != "startup" {
		t.Errorf("run triggers = %v, want the first labelled startup", triggers)
	}
}

// assertStartupRunSkipped boots the daemon and requires a not-due boot:
// healthy immediately, the skip explained in the log with the inherited
// record time, and no Renovate invocation at all.
func assertStartupRunSkipped(t *testing.T, rec *capture.Recorder, seedTime time.Time) {
	t.Helper()
	var argsLog [][]string
	cancel, done, runErr := startDaemonForTest(t, recordingRunner("true", &argsLog))

	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "daemon did not boot healthy on a fresh successful record")

	cancel()
	awaitDaemonStopped(t, done)
	if err := *runErr; err != nil {
		t.Errorf("runDaemon() = %v, want nil", err)
	}
	if len(argsLog) != 0 {
		t.Errorf("runner invoked %d times, want 0 (the startup run must be skipped)", len(argsLog))
	}
	if triggers := startTriggers(rec); len(triggers) != 0 {
		t.Errorf("run triggers = %v, want none", triggers)
	}
	skip, found := findRecord(rec, startupSkippedMsg)
	if !found {
		t.Fatalf("log records carry no %q line; the operator has no explanation for the missing startup run", startupSkippedMsg)
	}
	lastSuccess, ok := timeAttr(skip, "last_success")
	if !ok {
		t.Fatal("startup-skipped record carries no last_success attribute")
	}
	if !lastSuccess.Equal(seedTime) {
		t.Errorf("startup-skipped last_success = %v, want the inherited record time %v", lastSuccess, seedTime)
	}
}

// startDaemonForTest boots runDaemon in a goroutine on a fresh socket and
// returns the shutdown trigger, the done channel, and where the run error
// lands once done is closed.
func startDaemonForTest(t *testing.T, runner scheduler.CommandRunner) (cancel context.CancelFunc, done <-chan struct{}, runErr *error) {
	t.Helper()
	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	d := make(chan struct{})
	var err error
	go func() {
		defer close(d)
		err = runDaemon(ctx, sock, runner)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Error("runDaemon did not stop during test cleanup")
		}
	})
	return cancel, d, &err
}

func awaitDaemonStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return after shutdown")
	}
}

// findRecord returns the first captured record with the given message.
func findRecord(rec *capture.Recorder, msg string) (slog.Record, bool) {
	for _, r := range rec.Records() {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// timeAttr extracts a time-valued attribute from a record.
func timeAttr(r slog.Record, key string) (time.Time, bool) {
	var out time.Time
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key && a.Value.Kind() == slog.KindTime {
			out, found = a.Value.Time(), true
			return false
		}
		return true
	})
	return out, found
}

// TestExecutor_RecordsScheduledRunOutcomes pins which runs write the
// last-run record: scheduled triggers record their outcome, and a triggered
// client run never records — it is scoped to its own repos and environment,
// so it does not answer the full-pass freshness question a startup fire
// exists for.
func TestExecutor_RecordsScheduledRunOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		trig      string
		bin       string
		wantKnown bool
		wantOK    bool
	}{
		{"startup success records ok", "startup", "true", true, true},
		{"interval success records ok", "interval", "true", true, true},
		{"interval failure records failed", "interval", "false", true, false},
		{"triggered run records nothing", "external", "true", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			d, _, _ := newTestDaemon(t, recordingRunner(tt.bin, nil))

			out := submitWait(t, d, newJob(tt.trig, nil, nil))

			if wantOK := tt.bin == "true"; out.OK != wantOK {
				t.Fatalf("run outcome ok = %v, want %v", out.OK, wantOK)
			}
			rec, known := scheduler.NewStamp(d.stampPath).Last()
			if known != tt.wantKnown {
				t.Fatalf("stamp record known = %v, want %v", known, tt.wantKnown)
			}
			if known && rec.OK != tt.wantOK {
				t.Errorf("stamp record ok = %v, want %v", rec.OK, tt.wantOK)
			}
		})
	}
}

// TestExecutor_CancelledRunRecordsNothing pins the cancellation path: a run
// reaped by the shutdown handshake is not a completed run, so it must leave
// the last-run record untouched — the next boot fires the startup run.
func TestExecutor_CancelledRunRecordsNothing(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	ctx := t.Context()
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	d.runOnce = func(context.Context, stopRequested, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome {
		return runCancelled
	}

	j := newJob("interval", nil, nil)
	d.execute(context.WithoutCancel(ctx), ctx.Err, j)

	select {
	case <-j.Result():
	default:
		t.Fatal("no result delivered for the cancelled run")
	}
	if _, known := scheduler.NewStamp(d.stampPath).Last(); known {
		t.Error("cancelled run wrote a last-run record, want none (a cancelled run is not a completed run)")
	}
}

// TestExecutor_ContainmentRecordsFailure pins the halt path: a run whose
// process group survived the kill sweep overwrites even a fresh success with
// a failed record, so the boot after the halt treats the schedule as due.
func TestExecutor_ContainmentRecordsFailure(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	ctx := t.Context()
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	seedStamp(t, d.stampPath, time.Now(), "ok") // a fresh success the halt must displace
	d.runOnce = func(context.Context, stopRequested, time.Duration, string, runPayload, scheduler.CommandRunner) runOutcome {
		return runContained
	}

	j := newJob("interval", nil, nil)
	d.execute(context.WithoutCancel(ctx), ctx.Err, j)

	select {
	case <-j.Result():
	default:
		t.Fatal("no result delivered for the contained run")
	}
	rec, known := scheduler.NewStamp(d.stampPath).Last()
	if !known {
		t.Fatal("stamp record unreadable after a containment halt, want a failed record")
	}
	if rec.OK {
		t.Error("stamp record ok = true after a containment halt, want false (the next boot must treat the schedule as due)")
	}
}

// TestExecutor_RecordFailureWarnsAndKeepsRunResult pins the degradation of
// the record write itself: a stamp that cannot be written costs one Warn
// naming the path and one redundant future startup run, never the run's own
// outcome. Not parallel: it swaps the global slog default to capture records.
func TestExecutor_RecordFailureWarnsAndKeepsRunResult(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	rec := capture.Default(t)
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	gone := filepath.Join(t.TempDir(), "gone", stampName) // missing parent: every write fails
	d.stamp = scheduler.NewStamp(gone)
	d.stampPath = gone
	ctx := t.Context()

	j := newJob("interval", nil, nil)
	d.execute(context.WithoutCancel(ctx), ctx.Err, j)

	select {
	case out := <-j.Result():
		if !out.OK {
			t.Errorf("run outcome ok = false, want true (a failed record write must not fail the run)")
		}
	default:
		t.Fatal("no result delivered for the run")
	}
	warn, found := findRecord(rec, "cannot record the run outcome; next boot fires a startup run")
	if !found {
		t.Fatal("no record-failure warning logged, want one naming the stamp path")
	}
	if warn.Level != slog.LevelWarn {
		t.Errorf("record-failure log level = %v, want %v", warn.Level, slog.LevelWarn)
	}
	path, ok := stringAttr(warn, "path")
	if !ok || path != gone {
		t.Errorf("record-failure warning path = %q (present %v), want %q", path, ok, gone)
	}
}

// stringAttr extracts a string-valued attribute from a record.
func stringAttr(r slog.Record, key string) (string, bool) {
	var out string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out, found = a.Value.String(), true
			return false
		}
		return true
	})
	return out, found
}
