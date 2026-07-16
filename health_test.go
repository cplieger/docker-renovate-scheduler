package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
)

// TestProbeOptions_ArmsMaxAgeOnlyInBuiltinMode pins the probe freshness
// policy: built-in mode arms a max-age deadline (a wedged interval loop must
// probe unhealthy), external mode does not (an idle container between sparse
// triggers stays healthy indefinitely).
func TestProbeOptions_ArmsMaxAgeOnlyInBuiltinMode(t *testing.T) {
	t.Setenv("SCHED_TIMEOUT", "1h")

	t.Setenv("SCHED_INTERVAL", "6h")
	if opts := probeOptions(); len(opts) != 1 {
		t.Errorf("built-in mode probe options = %d, want 1 (max-age armed)", len(opts))
	}

	t.Setenv("SCHED_INTERVAL", "off")
	if opts := probeOptions(); len(opts) != 0 {
		t.Errorf("external mode probe options = %d, want 0 (no deadline on trigger-written markers)", len(opts))
	}
}

// TestProbe_WedgedBuiltinLoopFailsFreshness proves the deadman end-to-end
// with the real probe: a marker older than the armed max-age probes unhealthy
// in built-in mode, while external mode accepts a marker of any age.
func TestProbe_WedgedBuiltinLoopFailsFreshness(t *testing.T) {
	t.Setenv("SCHED_TIMEOUT", "1s")
	marker := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("setup marker: %v", err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(marker, stale, stale); err != nil {
		t.Fatalf("age marker: %v", err)
	}

	t.Setenv("SCHED_INTERVAL", "1m") // max age = 2m + 1s, marker is 1h old
	if code := health.ProbeCheck(marker, probeOptions()...); code == 0 {
		t.Error("stale marker probed healthy in built-in mode; the wedged-loop deadman is not armed")
	}

	t.Setenv("SCHED_INTERVAL", "off")
	if code := health.ProbeCheck(marker, probeOptions()...); code != 0 {
		t.Error("old marker probed unhealthy in external mode; idle containers must stay healthy")
	}
}
