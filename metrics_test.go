package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/scheduler/trigger"
)

func TestMetricsListenAddrTrimsInvisibleEdges(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{name: "leading space", raw: " :9470", want: ":9470"},
		{name: "zero-width edges", raw: "\u200b127.0.0.1:9470\ufeff", want: "127.0.0.1:9470"},
		{name: "blank", raw: " \t", want: ""},
		{name: "unset", raw: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("METRICS_LISTEN_ADDR", tt.raw)
			if got := metricsListenAddr(); got != tt.want {
				t.Errorf("metricsListenAddr() with %q = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// series reads every sample on reg, keyed name{label values joined by ","}.
func series(t *testing.T, reg *measure.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string]float64)
	for _, f := range families {
		for _, s := range f.Samples {
			vals := make([]string, 0, len(s.Labels))
			for _, l := range s.Labels {
				vals = append(vals, l.Value)
			}
			out[f.Name+"{"+strings.Join(vals, ",")+"}"] = s.Value
		}
	}
	return out
}

func seriesKey(name string, labels ...string) string {
	return metricsPrefix + "_" + name + "{" + strings.Join(labels, ",") + "}"
}

// daemonEnv sets the environment runDaemon reads to an external-mode daemon
// with token auth on a fresh base dir.
func daemonEnv(t *testing.T, metricsAddr string) {
	t.Helper()
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("RUN_INTERVAL", "off")
	t.Setenv("METRICS_LISTEN_ADDR", metricsAddr)
	for _, k := range []string{envAppID, envAppInstallationID, envAppKeyFile, envAppKeyInline} {
		t.Setenv(k, "")
	}
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
}

// startDaemon runs runDaemon until the test ends and waits until its trigger
// socket exists.
func startDaemon(t *testing.T) string {
	t.Helper()
	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, sock, recordingRunner("true", nil)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("runDaemon() = %v, want nil", err)
		}
	})
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "daemon did not bind the trigger socket")
	return sock
}

// getMetrics polls GET /metrics on addr until the daemon answers, and returns
// the status and the body.
func getMetrics(t *testing.T, addr string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	var status int
	var body []byte
	waitFor(t, 5*time.Second, func() bool {
		resp, err := client.Get("http://" + addr + "/metrics")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		status = resp.StatusCode
		body, _ = io.ReadAll(resp.Body)
		return true
	}, "GET /metrics never answered on "+addr)
	return status, string(body)
}

// tcpListeners counts this process's listening TCP sockets, read from the
// kernel's socket tables, so a test sees a bind however the code makes it.
func tcpListeners(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	ours := map[string]bool{}
	for _, fd := range fds {
		target, err := os.Readlink("/proc/self/fd/" + fd.Name())
		if inode, ok := strings.CutPrefix(target, "socket:["); err == nil && ok {
			ours[strings.TrimSuffix(inode, "]")] = true
		}
	}
	const stateListen = "0A"
	n := 0
	for _, table := range []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"} {
		raw, err := os.ReadFile(table)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range strings.Split(string(raw), "\n")[1:] {
			if f := strings.Fields(row); len(f) > 9 && f[3] == stateListen && ours[f[9]] {
				n++
			}
		}
	}
	return n
}

// freeAddr returns a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestDaemonWithoutMetricsAddressOpensNoListener: unset, the image opens no
// port. Not parallel: it uses the package-global health marker path.
func TestDaemonWithoutMetricsAddressOpensNoListener(t *testing.T) {
	daemonEnv(t, "")
	before := tcpListeners(t)
	sock := startDaemon(t)
	if code := runClient(sock, nil); code != 0 {
		t.Errorf("runClient() = %d, want 0", code)
	}
	if got := tcpListeners(t) - before; got != 0 {
		t.Errorf("the daemon opened %d TCP listeners with METRICS_LISTEN_ADDR unset, want none", got)
	}
}

// TestDaemonServesMetricsOnATrimmedAddress binds the address with its
// invisible edges trimmed and serves the page. Not parallel: it uses the
// package-global health marker path.
func TestDaemonServesMetricsOnATrimmedAddress(t *testing.T) {
	addr := freeAddr(t)
	daemonEnv(t, " \u200b"+addr)
	startDaemon(t)
	client := &http.Client{Timeout: 5 * time.Second}

	status, body := getMetrics(t, addr)
	want := `renovate_scheduler_info{auth="token",mode="external"} 1`
	if status != http.StatusOK || !strings.Contains(body, want) {
		t.Errorf("GET /metrics = %d with %q, want 200 with %s", status, body, want)
	}

	resp, err := client.Post("http://"+addr+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", resp.StatusCode)
	}
}

// TestDaemonRefusesAMetricsAddressItCannotBind: a malformed or taken address
// stops the daemon before it serves anything, with one error naming
// METRICS_LISTEN_ADDR. Not parallel: it swaps slog.Default.
func TestDaemonRefusesAMetricsAddressItCannotBind(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	for name, addr := range map[string]string{"malformed": "not-a-port", "taken": taken.Addr().String()} {
		t.Run(name, func(t *testing.T) {
			daemonEnv(t, addr)
			read := logLines(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := runDaemon(ctx, testSocketPath(t), recordingRunner("true", nil)); err == nil {
				t.Fatalf("runDaemon with METRICS_LISTEN_ADDR=%q returned nil, want an error", addr)
			}
			var naming int
			for _, l := range read() {
				if strings.Contains(l, "level=ERROR") && strings.Contains(l, `msg="cannot listen on METRICS_LISTEN_ADDR"`) {
					naming++
				}
				if strings.Contains(l, `msg="container started"`) {
					t.Errorf("the daemon started before the bind failed: %s", l)
				}
			}
			if naming != 1 {
				t.Errorf("ERROR lines naming METRICS_LISTEN_ADDR = %d, want 1:\n%s", naming, strings.Join(read(), "\n"))
			}
			if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("health marker after a refused start: stat err = %v, want not-exist (unhealthy)", err)
			}
		})
	}
}

// TestRunSetsEverySeriesToItsRecord runs each completed path and checks that
// its scope's series equal the fields of the run finished record it wrote.
func TestRunSetsEverySeriesToItsRecord(t *testing.T) {
	for i := range runPaths {
		p := &runPaths[i]
		if !p.completed {
			continue
		}
		t.Run(p.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			d, _ := newBareDaemon(t, recordingRunner("true", nil))
			reg := measure.NewRegistry(metricsPrefix)
			booted := time.Now().Add(-time.Minute)
			const gap = time.Hour
			d.metrics = newRunMetrics(reg, "external", "token", runBoot{start: booted, gap: gap})
			records := runOn(t, d, p)
			if len(records) != 1 {
				t.Fatalf("wrote %d run finished records, want 1", len(records))
			}
			rec, got := records[0], series(t, reg)
			scope := rec["scope"]
			num := func(field string) float64 {
				v, err := strconv.ParseFloat(rec[field], 64)
				if err != nil {
					t.Fatalf("record %s = %q: %v", field, rec[field], err)
				}
				return v
			}
			check := func(key string, want float64) {
				t.Helper()
				if g, ok := got[key]; !ok || g != want {
					t.Errorf("%s = %v (present %v), want %v", key, g, ok, want)
				}
			}
			check(seriesKey("last_run_ok", scope), num("ok"))
			check(seriesKey("last_run_exit_code", scope), num("exit_code"))
			for _, r := range runResults {
				want := float64(boolDigit(r == rec["result"]))
				check(seriesKey("last_run_result", scope, r), want)
				check(seriesKey("runs_total", scope, r), want)
			}
			if g := got[seriesKey("last_run_duration_seconds", scope)]; math.Abs(g-num("duration_ms")/1000) >= 0.001 {
				t.Errorf("last_run_duration_seconds = %v, record duration_ms = %s", g, rec["duration_ms"])
			}
			at, err := time.Parse(time.RFC3339Nano, rec["time"])
			if err != nil {
				t.Fatalf("record time %q: %v", rec["time"], err)
			}
			if g := got[seriesKey("last_run_timestamp_seconds", scope)]; math.Abs(g-float64(at.Unix())) > 1 {
				t.Errorf("last_run_timestamp_seconds = %v, record time %s", g, rec["time"])
			}
			success, ok := got[seriesKey("last_success_timestamp_seconds", scope)]
			if complete := rec["result"] == resultComplete; ok != complete || (complete && math.Abs(success-float64(at.Unix())) > 1) {
				t.Errorf("last_success_timestamp_seconds = %v (present %v), want the record's time only for a complete run", success, ok)
			}
			deadline := got[seriesKey("run_deadline_timestamp_seconds")]
			if rec["result"] == resultComplete && scope == scopeFull {
				if math.Abs(deadline-float64(at.Add(gap).Unix())) > 1 {
					t.Errorf("run_deadline_timestamp_seconds = %v after a complete full run, want one gap after the record time %s", deadline, rec["time"])
				}
			} else if want := float64(booted.Add(gap).Unix()); deadline != want {
				t.Errorf("run_deadline_timestamp_seconds = %v after a %s %s run, want the boot deadline %v", deadline, scope, rec["result"], want)
			}
			clock := got[seriesKey("failing_since_timestamp_seconds", scope)]
			if failing := rec["result"] == resultRepositoryError || rec["result"] == resultTokenUnavailable; failing != (clock > 0) {
				t.Errorf("failing_since_timestamp_seconds = %v for result %s", clock, rec["result"])
			}
			check(seriesKey("info", "external", "token"), 1)
			if v, ok := got[seriesKey("healthy")]; ok {
				t.Errorf("the page serves %s = %v; the health verdict is /healthz's, not a series", seriesKey("healthy"), v)
			}
		})
	}
}

// TestRunDeadlineAtStart pins the deadline a fresh page serves: one gap after
// the complete full run on record, or after the start when there is none or
// the record shows a failure, and 0 with no gap (external mode).
func TestRunDeadlineAtStart(t *testing.T) {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ran := start.Add(-5 * time.Hour)
	const gap = 13 * time.Hour
	good := scheduler.RunRecord{Time: ran, OK: true}
	bad := scheduler.RunRecord{Time: ran}
	tests := []struct {
		name        string
		boot        runBoot
		want        float64
		lastSuccess bool
	}{
		{name: "no record", boot: runBoot{start: start, gap: gap}, want: float64(start.Add(gap).Unix())},
		{name: "a complete run on record", boot: runBoot{start: start, gap: gap, last: good, known: true}, want: float64(ran.Add(gap).Unix()), lastSuccess: true},
		{name: "a failed run on record", boot: runBoot{start: start, gap: gap, last: bad, known: true}, want: float64(start.Add(gap).Unix())},
		{name: "external mode", boot: runBoot{start: start, last: good, known: true}, want: 0, lastSuccess: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := measure.NewRegistry(metricsPrefix)
			newRunMetrics(reg, "built-in", "token", tt.boot)
			got := series(t, reg)
			if g, ok := got[seriesKey("run_deadline_timestamp_seconds")]; !ok || g != tt.want {
				t.Errorf("run_deadline_timestamp_seconds = %v (present %v), want %v", g, ok, tt.want)
			}
			if _, ok := got[seriesKey("last_success_timestamp_seconds", scopeFull)]; ok != tt.lastSuccess {
				t.Errorf("full-scope last_success_timestamp_seconds present = %v, want %v", ok, tt.lastSuccess)
			}
		})
	}
}

// TestRunDeadlineMovesOnlyOnACompleteFullRun: a complete full run dates the
// deadline one gap after it ends, and a failed or tolerated full run or a
// complete scoped run after it leaves it there.
func TestRunDeadlineMovesOnlyOnACompleteFullRun(t *testing.T) {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const gap = 13 * time.Hour
	reg := measure.NewRegistry(metricsPrefix)
	m := newRunMetrics(reg, "built-in", "token", runBoot{start: start, gap: gap})
	deadline := func() float64 { return series(t, reg)[seriesKey("run_deadline_timestamp_seconds")] }

	ended := start.Add(time.Hour)
	m.observe(scopeFull, runVerdict{result: resultComplete, ok: true}, time.Minute, ended, time.Time{})
	want := float64(ended.Add(gap).Unix())
	if got := deadline(); got != want {
		t.Fatalf("deadline after a complete full run = %v, want %v", got, want)
	}
	for i, step := range []struct {
		name, scope string
		v           runVerdict
	}{
		{name: "a failed full run", scope: scopeFull, v: runVerdict{result: resultFailed, exitCode: 1}},
		{name: "a tolerated full run", scope: scopeFull, v: runVerdict{result: resultRepositoryError, ok: true, tolerated: true}},
		{name: "a complete scoped run", scope: scopeScoped, v: runVerdict{result: resultComplete, ok: true}},
	} {
		m.observe(step.scope, step.v, time.Minute, ended.Add(time.Duration(i+1)*time.Hour), time.Time{})
		if got := deadline(); got != want {
			t.Errorf("deadline after %s = %v, want it left at %v", step.name, got, want)
		}
	}
}

// sampleValue reads the unlabelled sample name on a metrics page body.
func sampleValue(t *testing.T, body, name string) float64 {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("%s = %q: %v", name, v, err)
			}
			return f
		}
	}
	t.Fatalf("the metrics page has no %s sample:\n%s", name, body)
	return 0
}

// TestMetricsPageServesBuildInfo pins the series a rule joins on to find this
// app's targets whatever their scrape job is named: one line at 1, labelled
// with the stamped version.
func TestMetricsPageServesBuildInfo(t *testing.T) {
	reg, _ := newMetricsRegistry("built-in", "token", scheduler.NewStamp(filepath.Join(t.TempDir(), "completed")), time.Hour)
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	var lines []string
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, metricsPrefix+"_build_info") {
			lines = append(lines, line)
		}
	}
	if want := metricsPrefix + `_build_info{version="` + version + `"} 1`; !slices.Equal(lines, []string{want}) {
		t.Errorf("build_info lines = %q, want [%q]", lines, want)
	}
}

// TestRejectedRunsCountByReason counts a refused scheduled run under the
// reason the queue gave.
func TestRejectedRunsCountByReason(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		queue        func() *trigger.Queue[runPayload]
	}{
		{name: "queue full", reason: rejectFull, queue: func() *trigger.Queue[runPayload] { return trigger.NewQueue[runPayload](0) }},
		{name: "shutting down", reason: rejectClosing, queue: func() *trigger.Queue[runPayload] {
			q := trigger.NewQueue[runPayload](4)
			q.Close()
			return q
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := measure.NewRegistry(metricsPrefix)
			d := &daemon{queue: tt.queue(), metrics: newRunMetrics(reg, "built-in", "token", runBoot{})}
			d.tick("interval")
			got := series(t, reg)
			for _, r := range []string{rejectFull, rejectClosing} {
				want := float64(boolDigit(r == tt.reason))
				if g := got[seriesKey("trigger_rejected_total", r)]; g != want {
					t.Errorf("trigger_rejected_total{%s} = %v, want %v", r, g, want)
				}
			}
		})
	}
}

// TestRestartSeedsLastSuccessFromTheLastCompleteFullRun: after a restart,
// before any run, the full scope's last-success series equals the time of
// the last complete full run of the previous process, which a later failed
// or scoped run does not move. Not parallel: it swaps slog.Default and uses
// the package-global health marker path.
func TestRestartSeedsLastSuccessFromTheLastCompleteFullRun(t *testing.T) {
	addr := freeAddr(t)
	daemonEnv(t, addr)
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	d.completed = scheduler.NewStamp(completedStampPath())
	for _, p := range []runPath{
		{name: "complete", setup: shellRunner("exit 0")},
		{name: "failed", setup: shellRunner("exit 1")},
		{name: "scoped", repos: []string{"owner/repo"}, setup: shellRunner("exit 0")},
	} {
		runOn(t, d, &p)
		d.queue = trigger.NewQueue[runPayload](queueCapacity)
		d.newCmd = recordingRunner("true", nil)
	}
	rec, known := scheduler.NewStamp(completedStampPath()).Last()
	if !known || !rec.OK {
		t.Fatalf("complete-run record = %+v known %v, want a good run", rec, known)
	}

	startDaemon(t)
	_, body := getMetrics(t, addr)
	want := fmt.Sprintf(`renovate_scheduler_last_success_timestamp_seconds{scope="full"} %d`, rec.Time.Unix())
	if !strings.Contains(body, want+"\n") {
		t.Errorf("metrics page after a restart lacks %q:\n%s", want, body)
	}
	if strings.Contains(body, `renovate_scheduler_last_success_timestamp_seconds{scope="scoped"}`) {
		t.Error("the scoped last-success series is seeded after a restart, want it absent until a scoped run")
	}
	if got := sampleValue(t, body, metricsPrefix+"_run_deadline_timestamp_seconds"); got != 0 {
		t.Errorf("run_deadline_timestamp_seconds in external mode = %v, want 0", got)
	}
}
