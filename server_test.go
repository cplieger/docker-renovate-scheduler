package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	scheduler "github.com/cplieger/scheduler/v2"
	"github.com/cplieger/scheduler/v2/trigger"
)

// The broker mechanics (queue semantics, socket hygiene, wire ordering,
// accept-loop degradation, departed clients) are the scheduler library's and
// are tested in scheduler/v2/trigger. These tests pin what stays THIS app's:
// the daemon executor's policy as observed over the real socket — scope and
// environment forwarding into the Renovate child, and shutdown's
// drain-versus-cancel split.

// startTestServer wires a queue + executor + trigger server on a temp socket
// and returns the socket path. Everything is torn down via t.Cleanup.
func startTestServer(t *testing.T, runner scheduler.CommandRunner) (sock string, d *daemon) {
	t.Helper()
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	sock = testSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	d, _ = newBareDaemon(t, ctx, runner)
	execDone := make(chan struct{})
	go func() { defer close(execDone); d.runJobs(ctx) }()

	ln, err := trigger.Listen(sock)
	if err != nil {
		t.Fatalf("trigger.Listen() = %v", err)
	}
	srv := &trigger.Server[runPayload]{Queue: d.queue}
	srv.Serve(ln)

	t.Cleanup(func() {
		_ = ln.Close()
		cancel()
		d.queue.Close()
		<-execDone
		srv.Wait()
	})
	return sock, d
}

// rawRequest dials the socket, sends a request, and returns the decoder over
// the event stream.
func rawRequest(t *testing.T, sock string, req runPayload) *json.Decoder {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("send request: %v", err)
	}
	return json.NewDecoder(conn)
}

// nextEvent decodes one event with a test deadline.
func nextEvent(t *testing.T, dec *json.Decoder) trigger.Event {
	t.Helper()
	var ev trigger.Event
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
		return trigger.Event{}
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

	dec := rawRequest(t, sock, runPayload{
		Repos: []string{"cplieger/homelab"},
		Env:   []string{"RENOVATE_TEST_MARKER=from-client", "RENOVATE_BASE_DIR=" + t.TempDir(), "PATH=" + os.Getenv("PATH")},
	})
	var final trigger.Event
	for {
		ev := nextEvent(t, dec)
		if ev.Kind == trigger.EventDone {
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

// TestServer_ShutdownCancelsQueuedRequestWithExplicitResult pins the
// shutdown contract on the wire: a request queued behind an in-flight run
// receives done{ok:false, reason: cancelled…} when the daemon stops — the
// trigger reports a failed job instead of hanging or being silently dropped.
// The in-flight child pauses AFTER process start (readiness marker, then
// blocks until released), so the shutdown lands on a run already committed
// past runRenovateOnce's post-Start handshake and A drains with its real
// outcome instead of being cancelled at start.
func TestServer_ShutdownCancelsQueuedRequestWithExplicitResult(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	sock := testSocketPath(t)

	runner, awaitEntered, release := gatedRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	d, _ := newBareDaemon(t, ctx, runner)
	execDone := make(chan struct{})
	go func() { defer close(execDone); d.runJobs(ctx) }()
	ln, err := trigger.Listen(sock)
	if err != nil {
		t.Fatalf("trigger.Listen() = %v", err)
	}
	srv := &trigger.Server[runPayload]{Queue: d.queue}
	srv.Serve(ln)

	// Occupy the executor, then queue a second request over the wire.
	decA := rawRequest(t, sock, runPayload{})
	awaitEntered() // A is executing, post-Start
	decB := rawRequest(t, sock, runPayload{Repos: []string{"owner/queued"}})
	if ev := nextEvent(t, decB); ev.Kind != trigger.EventQueued {
		t.Fatalf("B's first event = %q, want queued", ev.Kind)
	}

	// Daemon shutdown while A runs and B waits.
	cancel()
	_ = ln.Close()
	d.queue.Close()
	release() // A's child completes its pass

	// A: full drain — its run finished with its real (clean) outcome.
	for {
		ev := nextEvent(t, decA)
		if ev.Kind != trigger.EventDone {
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
		if ev.Kind != trigger.EventDone {
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
	srv.Wait()
}

// TestRunDaemon_FullQueueRejectsTriggerImmediately pins the documented
// backpressure contract end-to-end through the composition root: with the
// executor occupied by an in-flight run and the FIFO holding queueCapacity
// pending requests, the next trigger is rejected immediately — its FIRST
// wire event is a done with ok=false and a non-empty reason — instead of
// queueing unboundedly or hanging. This is also the only test that exercises
// runDaemon's OnRejected wiring. Not parallel: it uses the package-global
// healthMarkerPath.
func TestRunDaemon_FullQueueRejectsTriggerImmediately(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("SCHED_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	runner, awaitEntered, release := gatedRunner(t)

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runDaemon(ctx, sock, runner, nil)
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

	// Occupy the executor, then fill the queue: each fill request's queued
	// event is awaited before the next dial, so the fill is deterministic.
	_ = rawRequest(t, sock, runPayload{})
	awaitEntered()
	for i := range queueCapacity {
		dec := rawRequest(t, sock, runPayload{})
		if ev := nextEvent(t, dec); ev.Kind != trigger.EventQueued {
			t.Fatalf("fill request %d first event = %q, want queued", i, ev.Kind)
		}
	}

	dec := rawRequest(t, sock, runPayload{Repos: []string{"owner/overflow"}})
	ev := nextEvent(t, dec)
	if ev.Kind != trigger.EventDone {
		t.Fatalf("overflow request first event = %q, want an immediate done (reject-fast)", ev.Kind)
	}
	if ev.OK {
		t.Error("overflow request reported ok=true, want a rejection")
	}
	if ev.Reason == "" {
		t.Error("overflow rejection carries no reason; the trigger would report a bare failure")
	}
}
