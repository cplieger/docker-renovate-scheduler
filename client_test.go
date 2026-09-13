package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/scheduler/v4/trigger"
	"github.com/cplieger/slogx/capture"
)

// fakeDaemon runs serve off the test goroutine, so serve must not call t.Fatal.
func fakeDaemon(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	sock := testSocketPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		serve(conn)
	}()
	return sock
}

func TestRunClient_ExitCodesOverRealSocket(t *testing.T) {
	tests := []struct {
		name string
		bin  string
		want int
	}{
		{"clean run exits zero", "true", 0},
		{"failed run exits one", "false", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock := startTestServer(t, recordingRunner(tt.bin, nil))
			if code := runClient(sock, []string{"owner/repo"}); code != tt.want {
				t.Errorf("runClient() = %d, want %d", code, tt.want)
			}
		})
	}
}

func TestRunClient_DaemonUnreachableExitsOne(t *testing.T) {
	rec := capture.Default(t)
	sock := filepath.Join(t.TempDir(), "absent.sock")

	if code := runClient(sock, nil); code != 1 {
		t.Errorf("runClient() = %d with no daemon, want 1", code)
	}
	path, ok := rec.AttrValueExact("cannot reach the scheduler daemon", "path")
	if !ok {
		t.Fatalf("runClient() with no daemon recorded no unreachable diagnosis; captured: %v", rec.Messages())
	}
	if path != sock {
		t.Errorf("runClient() unreachable path = %q, want %q", path, sock)
	}
	hint, ok := rec.AttrValueExact("cannot reach the scheduler daemon", "hint")
	if !ok {
		t.Fatalf("runClient() with no daemon recorded no recovery hint; captured: %v", rec.Messages())
	}
	for _, want := range []string{"container is up", "container's user", "socket is owner-only"} {
		if !strings.Contains(hint, want) {
			t.Errorf("runClient() unreachable hint = %q, want it to name %q", hint, want)
		}
	}
}

func TestRunClient_ForwardsItsEnvironment(t *testing.T) {
	t.Setenv("RENOVATE_TEST_MARKER", "exec-override")
	runner := shellAssertRunner(`[ "$RENOVATE_TEST_MARKER" = "exec-override" ]`)
	sock := startTestServer(t, runner)
	if code := runClient(sock, nil); code != 0 {
		t.Error("runClient() != 0: the client's environment did not reach the child")
	}
}

func TestRunClient_ForwardsRepositories(t *testing.T) {
	var argsLog [][]string
	sock := startTestServer(t, recordingRunner("true", &argsLog))

	if code := runClient(sock, []string{"owner/a", "owner/b"}); code != 0 {
		t.Errorf("runClient() = %d, want 0", code)
	}
	if len(argsLog) != 1 {
		t.Fatalf("runClient() invoked the runner %d times, want 1", len(argsLog))
	}
	if want := []string{"renovate", "owner/a", "owner/b"}; !slices.Equal(argsLog[0], want) {
		t.Errorf("runClient([owner/a owner/b]) child args = %v, want %v", argsLog[0], want)
	}
}

// TestRunClient_ConnectionLostMidRunExitsOne pins the daemon-died-mid-run
// failure mode: when the event stream ends before the final done event (the
// daemon crashed or was stopped while the triggered run waited), the client
// must exit 1 — the trigger reports a failed job — never hang or report
// success. The fake daemon accepts the request, streams the queued event,
// then drops the connection.
func TestRunClient_ConnectionLostMidRunExitsOne(t *testing.T) {
	sock := fakeDaemon(t, func(conn net.Conn) {
		var req runPayload
		_ = json.NewDecoder(conn).Decode(&req)
		_, _ = conn.Write([]byte(`{"event":"queued"}` + "\n"))
	})

	if code := runClient(sock, []string{"owner/repo"}); code != 1 {
		t.Errorf("runClient() = %d after the daemon dropped the connection mid-run, want 1", code)
	}
}

// TestRunClient_RequestSendFailureExitsOne pins the third client failure
// class: a request write that fails mid-send (trigger.ErrSend) must exit 1 --
// the trigger reports a failed job -- completing the exit-code contract next
// to the unreachable-daemon and connection-lost pins. The fake daemon
// accepts and closes without reading; a ~4 MiB env entry makes the request
// larger than the unix-socket buffers, so the in-flight write hits the
// closed peer (EPIPE) instead of completing into the kernel buffer.
func TestRunClient_RequestSendFailureExitsOne(t *testing.T) {
	t.Setenv("RENOVATE_TEST_HUGE", strings.Repeat("x", 4<<20))
	sock := fakeDaemon(t, func(net.Conn) {})

	if code := runClient(sock, nil); code != 1 {
		t.Errorf("runClient() = %d when the request write fails, want 1", code)
	}
}

// TestFinishResult_FailedRunLogsTheReasonOrItsFallback pins the reason
// attribute on the client's failure line -- the whole diagnostic an operator
// reading the trigger's own job log gets, since the run's own
// output goes to the container log stream. A reason the daemon sent is
// reported verbatim; a failure that arrived without one still points at the
// log stream instead of logging an empty reason. Serial: swaps slog.Default.
func TestFinishResult_FailedRunLogsTheReasonOrItsFallback(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantReason string
	}{
		{
			name:       "start_failure_reason",
			reason:     "failed: renovate could not be started (see the container log stream)",
			wantReason: "failed: renovate could not be started (see the container log stream)",
		},
		{
			name:       "timeout_reason",
			reason:     "failed: run exceeded RUN_TIMEOUT (1h0m0s)",
			wantReason: "failed: run exceeded RUN_TIMEOUT (1h0m0s)",
		},
		{
			name:       "missing_reason_fallback",
			reason:     "",
			wantReason: "renovate exited non-zero (see the container log stream)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)

			code := finishResult(trigger.Event{Reason: tt.reason, DurationMs: 42}, []string{"owner/repo"})

			if code != 1 {
				t.Errorf("finishResult(Event{OK: false, Reason: %q}) = %d, want 1", tt.reason, code)
			}
			got, ok := rec.AttrValueExact("triggered run failed", "reason")
			if !ok {
				t.Fatalf("finishResult(Event{OK: false, Reason: %q}) logged no reason attribute on the failure line; captured: %v", tt.reason, rec.Messages())
			}
			if got != tt.wantReason {
				t.Errorf("finishResult(Event{OK: false, Reason: %q}) logged reason = %q, want %q", tt.reason, got, tt.wantReason)
			}
		})
	}
}

// TestRunClient_InterruptBeforeAcceptanceReportsUnknownOutcome pins the
// operator diagnosis for an interrupt that lands BEFORE the daemon accepted
// the request: the client cannot claim the run continues there, and it must
// not diagnose a transport failure it did not have. Serial: capture.Default
// swaps slog.Default.
func TestRunClient_InterruptBeforeAcceptanceReportsUnknownOutcome(t *testing.T) {
	rec := capture.Default(t)

	t.Setenv("RENOVATE_TEST_HUGE", strings.Repeat("x", 16<<20))
	accepted := make(chan struct{})
	release := make(chan struct{})
	sock := fakeDaemon(t, func(net.Conn) {
		close(accepted)
		// Keep the connection open so the interrupt lands during the request write.
		<-release
	})
	t.Cleanup(func() { close(release) })

	done := make(chan int, 1)
	go func() { done <- runClient(sock, nil) }()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("runClient did not connect within 5s")
	}

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("os.FindProcess() = %v", err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatalf("Signal(os.Interrupt) = %v", err)
	}

	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("runClient() after an interrupt before acceptance = %d, want 1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runClient did not return after the interrupt")
	}

	if !rec.Contains("acceptance was not observed") {
		t.Errorf("runClient() interrupt records = %v, want the unknown-acceptance warning", rec.Messages())
	}
	if rec.Contains("cannot reach the scheduler daemon") || rec.Contains("cannot send run request") {
		t.Errorf("runClient() interrupt records = %v, must not diagnose a transport failure", rec.Messages())
	}
}

func TestRunClient_InterruptAfterAcceptanceReportsContinuingRun(t *testing.T) {
	rec := capture.Default(t)

	queued := make(chan struct{})
	release := make(chan struct{})
	sock := fakeDaemon(t, func(conn net.Conn) {
		var req runPayload
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		if _, err := conn.Write([]byte(`{"event":"queued"}` + "\n")); err != nil {
			return
		}
		close(queued)
		// Keep the accepted run outstanding across the interrupt.
		<-release
	})
	t.Cleanup(func() { close(release) })

	done := make(chan int, 1)
	go func() { done <- runClient(sock, []string{"owner/repo"}) }()

	select {
	case <-queued:
	case <-time.After(5 * time.Second):
		t.Fatal("fake daemon did not queue the request within 5s")
	}
	waitFor(t, 5*time.Second, func() bool {
		return rec.Contains("triggered run accepted")
	}, "runClient did not observe acceptance within 5s")

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("os.FindProcess() = %v", err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatalf("Signal(os.Interrupt) = %v", err)
	}

	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("runClient() after an interrupt following acceptance = %d, want 1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runClient did not return after the interrupt")
	}

	if !rec.Contains("the run the daemon accepted continues there") {
		t.Errorf("runClient() interrupt records = %v, want the accepted-run warning", rec.Messages())
	}
	if rec.Contains("acceptance was not observed") {
		t.Errorf("runClient() interrupt records = %v, must not report acceptance as unknown", rec.Messages())
	}
}

// TestRunClient_LogsDocumentedLifecycle pins the three lifecycle lines the
// README's external-scheduler section publishes, their order, and the
// attributes that let a trigger's own log tie them to its run. Serial:
// capture.Default swaps slog.Default.
func TestRunClient_LogsDocumentedLifecycle(t *testing.T) {
	rec := capture.Default(t)

	sock := startTestServer(t, recordingRunner("true", nil))
	if code := runClient(sock, []string{"owner/repo"}); code != 0 {
		t.Errorf("runClient() = %d, want 0", code)
	}

	msgs := rec.Messages()
	accepted := slices.Index(msgs, "triggered run accepted")
	started := slices.Index(msgs, "triggered run started")
	complete := slices.Index(msgs, "triggered run complete")
	if accepted < 0 || started < 0 || complete < 0 {
		t.Fatalf("runClient() lifecycle records = %v, want accepted, started and complete", msgs)
	}
	if accepted >= started || started >= complete {
		t.Errorf("runClient() lifecycle order = %v, want accepted before started before complete", msgs)
	}
	for _, msg := range []string{"triggered run accepted", "triggered run started", "triggered run complete"} {
		if !rec.AttrContains(msg, "repos", "owner/repo") {
			t.Errorf("runClient() %q record = %v, want a repos attr naming owner/repo", msg, msgs)
		}
	}
	if !rec.HasAttr("triggered run started", "logs", "full Renovate output is on the container log stream") {
		t.Errorf("runClient() started record = %v, want the full-output location attr", msgs)
	}
	if _, ok := rec.AttrValueExact("triggered run complete", "duration_ms"); !ok {
		t.Errorf("runClient() complete record = %v, want a duration_ms attr", msgs)
	}
}
