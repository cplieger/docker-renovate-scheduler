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

func TestProbe_OverflowingIntervalStillArmsTheDeadman(t *testing.T) {
	// RUN_INTERVAL has no ceiling, so an operator can set a cadence whose
	// doubled nanosecond count exceeds int64. A wrapped negative deadline is
	// indistinguishable from "no deadline" at WithMaxAge, which would disarm
	// the wedged-loop check on the very configuration that most needs it.
	t.Setenv("RUN_INTERVAL", "1500000h")
	t.Setenv("RUN_TIMEOUT", "1s")
	marker := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("setup marker: %v", err)
	}
	if got := health.Inspect(marker, probeOptions()...).MaxAge; got <= 0 {
		t.Errorf("probeOptions() with RUN_INTERVAL=1500000h armed MaxAge = %v, want a positive deadline", got)
	}
}

func TestProbe_BuiltinFreshnessUsesPublishedBoundary(t *testing.T) {
	t.Setenv("RUN_INTERVAL", "1m")
	t.Setenv("RUN_TIMEOUT", "10s")
	tests := []struct {
		name     string
		age      time.Duration
		wantCode int
	}{
		{name: "inside_boundary_stays_healthy", age: 2*time.Minute + 5*time.Second, wantCode: 0},
		{name: "outside_boundary_is_unhealthy", age: 2*time.Minute + 15*time.Second, wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "marker")
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatalf("setup marker: %v", err)
			}
			aged := time.Now().Add(-tt.age)
			if err := os.Chtimes(marker, aged, aged); err != nil {
				t.Fatalf("age marker: %v", err)
			}
			if got := health.ProbeCheck(marker, probeOptions()...); got != tt.wantCode {
				t.Errorf("health.ProbeCheck(marker aged %v) = %d, want %d", tt.age, got, tt.wantCode)
			}
		})
	}
}
