package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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
