package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
)

func TestProbe_WedgedBuiltinLoopFailsFreshness(t *testing.T) {
	t.Setenv("RUN_TIMEOUT", "1s")
	marker := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("setup marker: %v", err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(marker, stale, stale); err != nil {
		t.Fatalf("age marker: %v", err)
	}

	t.Setenv("RUN_INTERVAL", "1m") // max age = 2m + 1s, marker is 1h old
	if code := health.ProbeCheck(marker, probeOptions()...); code == 0 {
		t.Error("stale marker probed healthy in built-in mode; the wedged-loop deadman is not armed")
	}

	t.Setenv("RUN_INTERVAL", "off")
	if code := health.ProbeCheck(marker, probeOptions()...); code != 0 {
		t.Error("old marker probed unhealthy in external mode; idle containers must stay healthy")
	}
}

func TestProbeOptions_ExtremeIntervalSaturatesDeadline(t *testing.T) {
	t.Setenv("RUN_TIMEOUT", "1h")
	t.Setenv("RUN_INTERVAL", "9223372036854775807ns") // time.Duration max: 2*interval wraps to -2

	marker := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("setup marker: %v", err)
	}
	aged := time.Now().Add(-2 * time.Hour) // older than the ~1h max-age an overflow would produce
	if err := os.Chtimes(marker, aged, aged); err != nil {
		t.Fatalf("age marker: %v", err)
	}

	opts := probeOptions()
	if len(opts) != 1 {
		t.Fatalf("probe options = %d, want 1 (built-in mode must keep the deadline armed, saturated, not disabled)", len(opts))
	}
	if code := health.ProbeCheck(marker, opts...); code != 0 {
		t.Error("marker probed unhealthy under an extreme RUN_INTERVAL; the saturation guard must keep the max-age effectively infinite instead of overflowing to a small positive deadline")
	}
}
