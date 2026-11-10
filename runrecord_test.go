package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/scheduler/trigger"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/go/slogx/capture"
)

// logLines swaps slog.Default for the production text handler at debug and
// returns a function that reads every line written so far.
func logLines(t *testing.T) func() []string {
	t.Helper()
	var buf bytes.Buffer
	h, _ := slogx.NewHandler(slogx.Options{Output: &buf, Level: slog.LevelDebug})
	capture.SwapDefault(t, slog.New(h))
	return func() []string {
		return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	}
}

// parseLogfmt splits one slog text line into its fields; a repeated key keeps
// its last value.
func parseLogfmt(t *testing.T, line string) map[string]string {
	t.Helper()
	fields := make(map[string]string)
	for line != "" {
		key, rest, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("malformed logfmt near %q", line)
		}
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := 1
			for end < len(rest) && (rest[end] != '"' || rest[end-1] == '\\') {
				end++
			}
			unquoted, err := strconv.Unquote(rest[:end+1])
			if err != nil {
				t.Fatalf("unquoting %q: %v", rest[:end+1], err)
			}
			value, rest = unquoted, rest[end+1:]
		} else {
			value, rest, _ = strings.Cut(rest, " ")
		}
		fields[key] = value
		line = strings.TrimLeft(rest, " ")
	}
	return fields
}

// recordsNamed returns the fields of every line whose msg is msg.
func recordsNamed(t *testing.T, lines []string, msg string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, l := range lines {
		if l == "" {
			continue
		}
		if f := parseLogfmt(t, l); f["msg"] == msg {
			out = append(out, f)
		}
	}
	return out
}

// runPath is one way a run can end.
type runPath struct {
	setup     func(t *testing.T, d *daemon)
	name      string
	result    string
	repos     []string
	exitCode  int
	ok        bool
	tolerated bool
	healthy   bool
	completed bool
	shutdown  bool
}

func seamResult(res runResult) func(t *testing.T, d *daemon) {
	return func(_ *testing.T, d *daemon) {
		d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
			return res
		}
	}
}

func shellRunner(script string) func(t *testing.T, d *daemon) {
	return func(_ *testing.T, d *daemon) {
		d.newCmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", script)
		}
	}
}

func tokenServer(status int, body string) func(t *testing.T, d *daemon) {
	return func(t *testing.T, d *daemon) {
		d.tokens = fakeGitHub(t, testInstallation, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, status, body)
		}, forgeapi.WithRetries(0))
		d.installationID = testInstallation
	}
}

// rateLimitedOnce answers the first token request 429 with Retry-After: 1,
// GitHub's shortest stated wait, and every later one with then.
func rateLimitedOnce(then http.HandlerFunc) func(t *testing.T, d *daemon) {
	return func(t *testing.T, d *daemon) {
		var asked atomic.Int32
		d.tokens = fakeGitHub(t, testInstallation, func(w http.ResponseWriter, r *http.Request) {
			if asked.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusTooManyRequests, `{"message":"You have exceeded a secondary rate limit"}`)
				return
			}
			then(w, r)
		}, forgeapi.WithRetries(0))
		d.installationID = testInstallation
	}
}

// tokenExpiringSoon mints a token that leaves a run about two seconds once
// tokenExpiryMargin is kept, under a RUN_TIMEOUT of an hour.
func tokenExpiringSoon(t *testing.T, d *daemon) {
	expiry := time.Now().Add(tokenExpiryMargin + 3*time.Second).UTC().Format(time.RFC3339)
	tokenServer(http.StatusCreated, tokenBodyExpiring(expiry))(t, d)
	d.timeout = time.Hour
}

func pastGrace(setup func(t *testing.T, d *daemon)) func(t *testing.T, d *daemon) {
	return func(t *testing.T, d *daemon) {
		setup(t, d)
		d.failingSince = time.Now().Add(-failureGrace - time.Minute)
		d.scopedFailingSince = d.failingSince
	}
}

var runPaths = []runPath{
	{name: "complete", setup: shellRunner("exit 0"), completed: true, result: resultComplete, ok: true, healthy: true},
	{name: "complete scoped", repos: []string{"owner/repo"}, setup: shellRunner("exit 0"), completed: true, result: resultComplete, ok: true, healthy: true},
	{name: "renovate exits non-zero", setup: shellRunner("exit 1"), completed: true, result: resultFailed, exitCode: 1},
	{name: "renovate aborts", setup: shellRunner("exit 134"), completed: true, result: resultAborted, exitCode: 134},
	{name: "renovate cannot start", setup: func(t *testing.T, d *daemon) {
		missing := filepath.Join(t.TempDir(), "missing")
		d.newCmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, missing) }
	}, completed: true, result: resultStartFailed, exitCode: noExitCode},
	{name: "run past RUN_TIMEOUT", setup: func(t *testing.T, d *daemon) {
		shellRunner("sleep 30")(t, d)
		d.timeout = 200 * time.Millisecond
	}, completed: true, result: resultTimedOut, exitCode: noExitCode},
	{name: "repository error inside the grace", setup: shellRunner("exit 7"), completed: true, result: resultRepositoryError, exitCode: 7, ok: true, tolerated: true, healthy: true},
	{name: "repository error past the grace", setup: pastGrace(shellRunner("exit 7")), completed: true, result: resultRepositoryError, exitCode: 7},
	{name: "process group survives", setup: seamResult(runResult{outcome: runContained, exitCode: 0}), completed: true, result: resultContained},
	{name: "base directory unwritable", setup: func(t *testing.T, _ *daemon) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RENOVATE_BASE_DIR", file)
	}, completed: true, result: resultPreflightFailed, exitCode: noExitCode},
	{name: "token outage inside the grace", setup: tokenServer(http.StatusServiceUnavailable, `{"message":"unavailable"}`), completed: true, result: resultTokenUnavailable, exitCode: noExitCode, ok: true, tolerated: true, healthy: true},
	{name: "token outage past the grace", setup: pastGrace(tokenServer(http.StatusServiceUnavailable, `{"message":"unavailable"}`)), completed: true, result: resultTokenUnavailable, exitCode: noExitCode},
	{name: "token rejected", setup: tokenServer(http.StatusUnauthorized, `{"message":"Bad credentials"}`), completed: true, result: resultTokenFailed, exitCode: noExitCode},
	{name: "token rate-limited, then complete", setup: func(t *testing.T, d *daemon) {
		rateLimitedOnce(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, tokenBody()) })(t, d)
		shellRunner("exit 0")(t, d)
	}, completed: true, result: resultComplete, ok: true, healthy: true},
	{name: "token rate-limited, then an outage", setup: rateLimitedOnce(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, `{"message":"unavailable"}`)
	}), completed: true, result: resultTokenUnavailable, exitCode: noExitCode, ok: true, tolerated: true, healthy: true},
	{name: "run past the token's life", setup: func(t *testing.T, d *daemon) {
		tokenExpiringSoon(t, d)
		shellRunner("sleep 30")(t, d)
	}, completed: true, result: resultTimedOut, exitCode: noExitCode},
	{name: "halted by a surviving group", setup: func(_ *testing.T, d *daemon) { d.halted = true }},
	{name: "shutdown before the run", shutdown: true},
}

// runThrough drives one path through the executor from a healthy daemon and
// returns the job's outcome, the run finished records it wrote, and whether
// health ended healthy.
func runThrough(t *testing.T, p *runPath) (trigger.Result, []map[string]string, bool) {
	t.Helper()
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	return runOn(t, d, p)
}

// runOn drives one path through d's executor, as runThrough does.
func runOn(t *testing.T, d *daemon, p *runPath) (trigger.Result, []map[string]string, bool) {
	t.Helper()
	read := logLines(t)
	d.health.Set(true)
	if p.setup != nil {
		p.setup(t, d)
	}
	j := newJob("external", p.repos, nil)
	if err := d.queue.Submit(j); err != nil {
		t.Fatal(err)
	}
	d.queue.Close()
	ctx, cancel := context.WithCancel(t.Context())
	if p.shutdown {
		cancel()
	}
	d.runJobs(ctx)
	cancel()
	out := <-j.Result()
	return out, recordsNamed(t, read(), runFinishedMsg), d.marker.Healthy()
}

// TestEveryCompletedRunWritesOneRunFinishedRecord is the record's contract:
// each completed run writes exactly one INFO run finished record, whose ok is
// the verdict the run client reports and whose health write matches it; a run
// that never ran writes none. Serial: swaps slog.Default.
func TestEveryCompletedRunWritesOneRunFinishedRecord(t *testing.T) {
	for i := range runPaths {
		p := &runPaths[i]
		t.Run(p.name, func(t *testing.T) {
			out, records, healthy := runThrough(t, p)
			if !p.completed {
				if len(records) != 0 {
					t.Errorf("wrote %d run finished records, want none: %v", len(records), records)
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("wrote %d run finished records, want 1: %v", len(records), records)
			}
			rec := records[0]
			want := map[string]string{
				"level": "INFO", "trigger": "external", "result": p.result,
				"ok": strconv.Itoa(boolDigit(p.ok)), "tolerated": strconv.Itoa(boolDigit(p.tolerated)),
				"exit_code": strconv.Itoa(p.exitCode), "scope": "full",
			}
			if len(p.repos) > 0 {
				want["scope"] = "scoped"
			}
			for k, v := range want {
				if rec[k] != v {
					t.Errorf("record %s = %q, want %q (record %v)", k, rec[k], v, rec)
				}
			}
			if _, err := strconv.ParseInt(rec["duration_ms"], 10, 64); err != nil {
				t.Errorf("record duration_ms = %q, not a number", rec["duration_ms"])
			}
			if got := rec["ok"] == "1"; got != out.OK {
				t.Errorf("record ok = %s, but the run client's outcome is ok=%v", rec["ok"], out.OK)
			}
			if healthy != p.healthy {
				t.Errorf("health after the run = %v, want %v", healthy, p.healthy)
			}
			if !p.tolerated && healthy != out.OK {
				t.Errorf("health = %v disagrees with the outcome ok=%v", healthy, out.OK)
			}
		})
	}
}

// TestARateLimitedRunCountsItsFailureFromTheWaitsEnd covers a run that waits
// out an hour-long rate limit and then fails in a way an outage causes: its
// one run finished record is tolerated and its duration includes the wait,
// while the failure clock (failing_since_timestamp_seconds and the failure
// line's failing_for) starts where the wait ended. Serial: swaps slog.Default.
func TestARateLimitedRunCountsItsFailureFromTheWaitsEnd(t *testing.T) {
	const wait = time.Hour
	tests := []struct {
		after   http.HandlerFunc
		name    string
		result  string
		line    string
		outcome runOutcome
	}{
		{
			name: "a token outage after the wait",
			after: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusServiceUnavailable, `{"message":"unavailable"}`)
			},
			outcome: runComplete, result: resultTokenUnavailable, line: "renovate run skipped: github app token unavailable",
		},
		{
			name:    "a repository error after the wait",
			after:   func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, tokenBody()) },
			outcome: runRepositoryError, result: resultRepositoryError, line: "renovate run ended in a repository error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				read := logLines(t)
				d, _ := newBareDaemon(t, nil)
				reg := measure.NewRegistry(metricsPrefix)
				d.metrics = newRunMetrics(reg, d.marker, "external", "app", scheduler.RunRecord{}, false)
				var asked atomic.Int32
				d.tokens = fakeGitHub(t, testInstallation, func(w http.ResponseWriter, r *http.Request) {
					if asked.Add(1) == 1 {
						w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
						writeJSON(w, http.StatusTooManyRequests, `{"message":"You have exceeded a secondary rate limit"}`)
						return
					}
					tt.after(w, r)
				}, forgeapi.WithRetries(0))
				d.installationID = testInstallation
				d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
					return runResult{outcome: tt.outcome}
				}
				d.health.Set(true)
				start := time.Now()

				j := newJob("interval", nil, nil)
				d.execute(t.Context(), t.Context().Done(), j)
				out := <-j.Result()
				lines := read()

				if !out.OK || out.Reason != toleratedReason {
					t.Errorf("outcome = ok %v reason %q, want ok with %q", out.OK, out.Reason, toleratedReason)
				}
				records := recordsNamed(t, lines, runFinishedMsg)
				if len(records) != 1 {
					t.Fatalf("wrote %d run finished records, want 1: %v", len(records), records)
				}
				rec := records[0]
				if rec["result"] != tt.result || rec["ok"] != "1" || rec["tolerated"] != "1" {
					t.Errorf("record result=%s ok=%s tolerated=%s, want %s, 1, 1", rec["result"], rec["ok"], rec["tolerated"], tt.result)
				}
				if ms, err := strconv.ParseInt(rec["duration_ms"], 10, 64); err != nil || ms < wait.Milliseconds() {
					t.Errorf("record duration_ms = %q, want at least the %s wait", rec["duration_ms"], wait)
				}
				failures := recordsNamed(t, lines, tt.line)
				if len(failures) != 1 {
					t.Fatalf("%q lines = %d, want 1", tt.line, len(failures))
				}
				if got := failingFor(t, failures[0]); got >= time.Minute {
					t.Errorf("%q failing_for = %s, want under a minute: the failures count from the wait's end", tt.line, got)
				}
				clock := series(t, reg)[seriesKey("failing_since_timestamp_seconds", scopeFull)]
				if end := float64(start.Add(wait).Unix()); clock < end || clock > end+60 {
					t.Errorf("failing_since_timestamp_seconds = %.0f, want the wait's end %.0f, not the run's start %d", clock, end, start.Unix())
				}
			})
		})
	}
}

// TestRunDaemonErrorReturnsLogDaemonStopped covers every error return of
// runDaemon: each writes exactly one ERROR daemon stopped line. Not parallel:
// it swaps slog.Default and uses the package-global health marker path.
func TestRunDaemonErrorReturnsLogDaemonStopped(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	cases := []struct {
		setup func(t *testing.T) string
		run   func(t *testing.T, sock string) error
		name  string
	}{
		{name: "github app configuration invalid", setup: func(t *testing.T) string {
			t.Setenv(envAppID, "not-a-number")
			t.Setenv(envAppKeyFile, writeKeyFile(t, false))
			return testSocketPath(t)
		}},
		{name: "trigger socket taken", setup: func(t *testing.T) string {
			sock := testSocketPath(t)
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			return sock
		}},
		{name: "base directory unwritable", setup: func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RENOVATE_BASE_DIR", file)
			return testSocketPath(t)
		}},
		{name: "metrics address malformed", setup: func(t *testing.T) string {
			t.Setenv("METRICS_LISTEN_ADDR", "not-a-port")
			return testSocketPath(t)
		}},
		{name: "process group survives", setup: testSocketPath, run: runContainedDaemon},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			t.Setenv("RUN_INTERVAL", "off")
			t.Setenv("METRICS_LISTEN_ADDR", "")
			for _, k := range []string{envAppID, envAppInstallationID, envAppKeyFile, envAppKeyInline} {
				t.Setenv(k, "")
			}
			sock := c.setup(t)
			rec := capture.Default(t)
			run := c.run
			if run == nil {
				run = func(t *testing.T, sock string) error {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					return runDaemon(ctx, sock, recordingRunner("true", nil))
				}
			}
			if err := run(t, sock); err == nil {
				t.Fatal("the daemon returned no error")
			}
			if n := rec.CountExact(daemonStoppedMsg); n != 1 {
				t.Errorf("%q lines = %d, want 1; captured: %v", daemonStoppedMsg, n, rec.Messages())
			}
			if n := rec.CountLevel(slog.LevelError, daemonStoppedMsg); n != 1 {
				t.Errorf("ERROR %q lines = %d, want 1", daemonStoppedMsg, n)
			}
			if v, ok := rec.AttrValueExact(daemonStoppedMsg, "error"); !ok || v == "" {
				t.Errorf("%q carries no error attribute", daemonStoppedMsg)
			}
		})
	}
}

// runContainedDaemon runs the daemon until a run's process group survives
// the sweep, which stops the daemon with an error.
func runContainedDaemon(t *testing.T, sock string) error {
	t.Helper()
	ln, err := trigger.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
		return runResult{outcome: runContained}
	}
	done := make(chan error, 1)
	go func() { done <- d.run(t.Context(), ln, sock, time.Hour, false, 0) }()
	if code := runClient(sock, nil); code != 1 {
		t.Errorf("runClient() = %d, want 1", code)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon did not stop after the containment loss")
		return nil
	}
}

// TestDaemonNeverLogsDaemonStoppedOnACleanShutdown keeps the line's meaning:
// a requested shutdown is not an abnormal exit. Not parallel: it swaps
// slog.Default.
func TestDaemonNeverLogsDaemonStoppedOnACleanShutdown(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	sock := testSocketPath(t)
	ln, err := trigger.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	rec := capture.Default(t)
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.run(ctx, ln, sock, time.Hour, false, 0); err != nil {
		t.Fatalf("run() after a requested shutdown = %v, want nil", err)
	}
	if n := rec.CountExact(daemonStoppedMsg); n != 0 {
		t.Errorf("%q lines after a requested shutdown = %d, want 0", daemonStoppedMsg, n)
	}
}
