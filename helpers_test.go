package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	scheduler "github.com/cplieger/scheduler/v3"
)

// shellAssertRunner returns a CommandRunner whose child is a shell running
// script — exit 0 iff the assertion holds — used to verify the child's real
// environment.
func shellAssertRunner(script string) scheduler.CommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

// TestWireEvent_OKIsExplicitOnTheWire pins the protocol regression guard: a
// done event always carries "ok" (a failed run must be explicit, not an
// omitted field a lenient decoder defaults).
func TestWireEvent_OKIsExplicitOnTheWire(t *testing.T) {
	t.Parallel()
	for _, ok := range []bool{true, false} {
		raw, err := json.Marshal(wireEvent{Event: eventDone, OK: ok})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"ok":`) {
			t.Errorf("wire form %s omits the ok field (ok=%v), want it explicit", raw, ok)
		}
	}
}

// TestWireRoundTrip pins request/event symmetry through JSON.
func TestWireRoundTrip(t *testing.T) {
	t.Parallel()
	req := wireRequest{Repos: []string{"a/b"}, Env: []string{"K=V"}}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got wireRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Repos) != 1 || got.Repos[0] != "a/b" || len(got.Env) != 1 || got.Env[0] != "K=V" {
		t.Errorf("round trip = %+v, want the original request", got)
	}
}
