package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/health"
	scheduler "github.com/cplieger/scheduler/v2"
)

// startTestServer wires a queue + executor + trigger server on a temp socket
// and returns the socket path. Everything is torn down via t.Cleanup.
func startTestServer(t *testing.T, runner scheduler.CommandRunner) (sock string, d *daemon) {
	t.Helper()
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	sock = filepath.Join(t.TempDir(), "trigger.sock")

	ctx, cancel := context.WithCancel(context.Background())
	d = &daemon{
		queue:   newRunQueue(queueCapacity),
		marker:  health.NewMarker(filepath.Join(t.TempDir(), "marker")),
		newCmd:  runner,
		runCtx:  context.WithoutCancel(ctx),
		timeout: time.Minute,
	}
	execDone := make(chan struct{})
	go func() { defer close(execDone); d.runJobs(ctx) }()

	ln, err := listenTrigger(sock)
	if err != nil {
		t.Fatalf("listenTrigger() = %v", err)
	}
	srv := &triggerServer{queue: d.queue}
	go srv.serve(ln)

	t.Cleanup(func() {
		_ = ln.Close()
		cancel()
		d.queue.close()
		<-execDone
		srv.handlers.Wait()
	})
	return sock, d
}

// TestListenTrigger_RemovesStaleSocketAndSetsOwnerOnly pins the boot hygiene:
// a stale socket file from a SIGKILLed predecessor is replaced, and the live
// socket is owner-only (triggering scoped to the container's user).
func TestListenTrigger_RemovesStaleSocketAndSetsOwnerOnly(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "trigger.sock")

	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("setup stale socket: %v", err)
	}
	// Simulate a SIGKILL: the file stays, nobody listens. Closing the
	// listener would remove the file, so leak it deliberately and only
	// unlink-guard via listenTrigger.
	_ = stale.(*net.UnixListener)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket file missing after setup: %v", err)
	}

	ln, err := listenTrigger(sock)
	if err != nil {
		t.Fatalf("listenTrigger() over a stale socket = %v, want nil", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat live socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket permissions = %o, want 0600 (owner-only trigger authority)", perm)
	}
}

// rawRequest dials the socket, sends a request, and returns the decoder over
// the event stream plus the connection for cleanup.
func rawRequest(t *testing.T, sock string, req wireRequest) (*json.Decoder, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("send request: %v", err)
	}
	return json.NewDecoder(conn), conn
}

// nextEvent decodes one event with a test deadline.
func nextEvent(t *testing.T, dec *json.Decoder) wireEvent {
	t.Helper()
	var ev wireEvent
	done := make(chan error, 1)
	go func() { done <- dec.Decode(&ev) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("decode event: %v", err)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return wireEvent{}
	}
}

// TestServer_EventSequenceForCleanRun pins the wire contract: queued →
// started → done{ok:true, duration>0}, in that order, one done exactly.
func TestServer_EventSequenceForCleanRun(t *testing.T) {
	sock, _ := startTestServer(t, recordingRunner("true", nil))
	dec, _ := rawRequest(t, sock, wireRequest{Repos: []string{"owner/repo"}})

	if ev := nextEvent(t, dec); ev.Event != eventQueued {
		t.Fatalf("first event = %q, want %q", ev.Event, eventQueued)
	}
	if ev := nextEvent(t, dec); ev.Event != eventStarted {
		t.Fatalf("second event = %q, want %q", ev.Event, eventStarted)
	}
	ev := nextEvent(t, dec)
	if ev.Event != eventDone || !ev.OK {
		t.Fatalf("final event = %+v, want done ok=true", ev)
	}
}

// TestServer_ForwardsScopeAndEnvironmentToTheRun is the rewrite's core
// regression test: the request's repos become the run's positional args and
// the request's env becomes the child's environment — the two things the old
// cross-process design could lose (scope) or not replay at all (env).
func TestServer_ForwardsScopeAndEnvironmentToTheRun(t *testing.T) {
	var argsLog [][]string
	runner := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		argsLog = append(argsLog, append([]string(nil), args...))
		return exec.CommandContext(ctx, "sh", "-c", `[ "$RENOVATE_TEST_MARKER" = "from-client" ]`)
	}
	sock, _ := startTestServer(t, runner)

	dec, _ := rawRequest(t, sock, wireRequest{
		Repos: []string{"cplieger/homelab"},
		Env:   []string{"RENOVATE_TEST_MARKER=from-client", "PATH=" + os.Getenv("PATH")},
	})
	var final wireEvent
	for {
		ev := nextEvent(t, dec)
		if ev.Event == eventDone {
			final = ev
			break
		}
	}
	if !final.OK {
		t.Error("done ok=false: the forwarded environment did not reach the child (the shell assertion failed)")
	}
	if len(argsLog) != 1 || !strings.Contains(strings.Join(argsLog[0], " "), "cplieger/homelab") {
		t.Errorf("run args = %v, want the request's positional repo", argsLog)
	}
}

// TestServer_FailedRunReportsNotOK pins the exit-code half of the trigger
// contract at the wire level.
func TestServer_FailedRunReportsNotOK(t *testing.T) {
	sock, _ := startTestServer(t, recordingRunner("false", nil))
	dec, _ := rawRequest(t, sock, wireRequest{})
	for {
		ev := nextEvent(t, dec)
		if ev.Event != eventDone {
			continue
		}
		if ev.OK {
			t.Error("done ok=true for a failing run, want false")
		}
		return
	}
}

// TestServer_RejectsWhenQueueFull pins honest backpressure: a full queue
// answers immediately with done{ok:false, reason} instead of queueing
// unboundedly or blocking the trigger.
func TestServer_RejectsWhenQueueFull(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	sock := filepath.Join(t.TempDir(), "trigger.sock")

	// No executor: jobs sit in the queue. Capacity 1, pre-filled.
	q := newRunQueue(1)
	if err := q.submit(newJob("external", nil, nil)); err != nil {
		t.Fatalf("pre-fill submit: %v", err)
	}
	ln, err := listenTrigger(sock)
	if err != nil {
		t.Fatalf("listenTrigger() = %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := &triggerServer{queue: q}
	go srv.serve(ln)

	dec, _ := rawRequest(t, sock, wireRequest{})
	ev := nextEvent(t, dec)
	if ev.Event != eventDone || ev.OK {
		t.Fatalf("event = %+v, want immediate done ok=false", ev)
	}
	if !strings.Contains(ev.Reason, "full") {
		t.Errorf("reason = %q, want a queue-full explanation", ev.Reason)
	}
}

// TestServer_UndecodableRequestAnswersDone pins the protocol's failure mode
// for a malformed client: an explicit done with a reason, never a hang.
func TestServer_UndecodableRequestAnswersDone(t *testing.T) {
	sock, _ := startTestServer(t, recordingRunner("true", nil))
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	dec := json.NewDecoder(conn)
	ev := nextEvent(t, dec)
	if ev.Event != eventDone || ev.OK {
		t.Fatalf("event = %+v, want done ok=false for an undecodable request", ev)
	}
}

// TestServer_ShutdownCancelsQueuedRequestWithExplicitResult pins the
// shutdown contract on the wire: a request queued behind an in-flight run
// receives done{ok:false, reason: cancelled…} when the daemon stops — the
// trigger reports a failed job instead of hanging or being silently dropped.
func TestServer_ShutdownCancelsQueuedRequestWithExplicitResult(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	sock := filepath.Join(t.TempDir(), "trigger.sock")

	entered := make(chan struct{})
	proceed := make(chan struct{})
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		close(entered)
		<-proceed
		return exec.CommandContext(ctx, "true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{
		queue:   newRunQueue(queueCapacity),
		marker:  health.NewMarker(filepath.Join(t.TempDir(), "marker")),
		newCmd:  runner,
		runCtx:  context.WithoutCancel(ctx),
		timeout: time.Minute,
	}
	execDone := make(chan struct{})
	go func() { defer close(execDone); d.runJobs(ctx) }()
	ln, err := listenTrigger(sock)
	if err != nil {
		t.Fatalf("listenTrigger() = %v", err)
	}
	srv := &triggerServer{queue: d.queue}
	go srv.serve(ln)

	// Occupy the executor, then queue a second request over the wire.
	decA, _ := rawRequest(t, sock, wireRequest{})
	<-entered
	decB, _ := rawRequest(t, sock, wireRequest{Repos: []string{"owner/queued"}})
	if ev := nextEvent(t, decB); ev.Event != eventQueued {
		t.Fatalf("B's first event = %q, want queued", ev.Event)
	}

	// Daemon shutdown while A runs and B waits.
	cancel()
	_ = ln.Close()
	d.queue.close()
	close(proceed) // A's child completes its pass

	// A: full drain — its run finished with its real (clean) outcome.
	for {
		ev := nextEvent(t, decA)
		if ev.Event != eventDone {
			continue
		}
		if !ev.OK {
			t.Error("in-flight run reported ok=false at shutdown, want true (drained, not abandoned)")
		}
		break
	}
	// B: explicit cancellation.
	for {
		ev := nextEvent(t, decB)
		if ev.Event != eventDone {
			continue
		}
		if ev.OK {
			t.Error("queued request reported ok=true at shutdown, want cancelled")
		}
		if !strings.Contains(ev.Reason, "shutting down") {
			t.Errorf("cancellation reason = %q, want a shutting-down explanation", ev.Reason)
		}
		break
	}

	<-execDone
	srv.handlers.Wait()
}
