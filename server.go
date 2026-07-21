package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// --- Trigger socket server ---
//
// The daemon listens on an in-container unix socket; each connection is one
// run request (see protocol.go). The socket replaces the whole cross-process
// /tmp coordination lattice of the previous design: mutual exclusion is the
// executor's single goroutine, shutdown reaches waiting clients as explicit
// cancellation results, and completion is an ordinary result delivery — no
// flock, no rerun flag, no drain latch, no lock polling.

const (
	// requestReadTimeout bounds how long a connected client may take to send
	// its request line, so a silent connection cannot hold a handler
	// goroutine (and shutdown) hostage.
	requestReadTimeout = 30 * time.Second
	// eventWriteTimeout bounds each status write, so a dead client cannot
	// block a handler.
	eventWriteTimeout = 10 * time.Second
	// maxRequestBytes caps one request line. A forwarded environ is
	// kernel-bounded to ~2 MiB per exec, so 8 MiB is generous headroom;
	// anything larger is a bug or abuse and is rejected as undecodable.
	maxRequestBytes = 8 << 20
)

// listenTrigger binds the unix socket at path with owner-only permissions.
// A stale socket file from a SIGKILLed predecessor is removed first (bind
// fails on an existing path otherwise); /tmp is per-container, so the stale
// file can only be our own previous life's.
func listenTrigger(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Narrow the umask so the socket is born owner-only; the Chmod below is
	// then belt-and-braces instead of closing a world-connectable window.
	// Safe: listenTrigger runs before any other goroutine creates files.
	oldMask := syscall.Umask(0o177)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	syscall.Umask(oldMask)
	if err != nil {
		return nil, err
	}
	// Owner-only: connecting requires write permission on the socket file,
	// which scopes triggering to the container's own user — the same
	// authority boundary `docker exec` already enforces, now failing loudly
	// at connect for a mismatched exec user.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// triggerServer accepts run requests and bridges them onto the queue.
type triggerServer struct {
	queue *runQueue
	// handlers tracks the accept loop plus per-connection goroutines so
	// shutdown can wait for every accepted request to receive its final
	// event before the daemon exits. The serve loop registers itself here
	// too (its caller uses handlers.Go), keeping the counter non-zero until
	// Accept has failed with net.ErrClosed -- so no handler Add can race
	// Wait at zero. Bounded: every submitted job is guaranteed a result,
	// and a not-yet-submitted connection is bounded by requestReadTimeout.
	handlers sync.WaitGroup
}

// serve accepts connections until the listener is closed (daemon shutdown).
func (s *triggerServer) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("trigger socket accept failed", "error", err)
			continue
		}
		s.handlers.Go(func() {
			defer func() { _ = conn.Close() }()
			s.handle(conn)
		})
	}
}

// handle serves one connection: decode the request, submit it, stream events.
func (s *triggerServer) handle(conn net.Conn) {
	var req wireRequest
	_ = conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	if err := json.NewDecoder(io.LimitReader(conn, maxRequestBytes)).Decode(&req); err != nil {
		slog.Warn("trigger request rejected: undecodable", "error", err)
		writeEvent(conn, wireEvent{Event: eventDone, OK: false, Reason: "undecodable request"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	j := newJob("external", req.Repos, req.Env)
	if err := s.queue.submit(j); err != nil {
		slog.Warn("trigger request rejected", "repos", req.Repos, "reason", err)
		writeEvent(conn, wireEvent{Event: eventDone, OK: false, Reason: err.Error()})
		return
	}
	slog.Info("triggered run queued", "repos", req.Repos)
	writeEvent(conn, wireEvent{Event: eventQueued})

	relayEvents(conn, j)
}

// relayEvents streams the job's lifecycle to the client: a started event if
// the run begins (a job cancelled before starting delivers its result without
// ever starting, so it waits on both), then exactly one final done event.
func relayEvents(conn net.Conn, j *job) {
	started := j.started
	for {
		select {
		case <-started:
			writeEvent(conn, wireEvent{Event: eventStarted})
			started = nil // block this case from now on; wait for the result
		case out := <-j.result:
			if started != nil {
				// Both channels can be ready in one select round (a fast
				// run), so drain the start signal first: the wire order the
				// protocol documents (queued -> started -> done) must hold
				// for a run that actually started.
				select {
				case <-started:
					writeEvent(conn, wireEvent{Event: eventStarted})
				default:
				}
			}
			writeEvent(conn, wireEvent{
				Event:      eventDone,
				OK:         out.ok,
				DurationMs: out.duration.Milliseconds(),
				Reason:     out.reason,
			})
			return
		}
	}
}

// writeEvent sends one status line, best-effort: a departed client only
// forfeits its own visibility (the run itself is daemon-owned and its result
// delivery never blocks on the connection).
func writeEvent(conn net.Conn, ev wireEvent) {
	_ = conn.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	if err := json.NewEncoder(conn).Encode(ev); err != nil {
		slog.Debug("trigger event write failed (client gone?)", "event", ev.Event, "error", err)
	}
}
