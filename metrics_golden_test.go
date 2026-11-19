package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/scheduler/v2"
	"github.com/cplieger/go/scheduler/v2/trigger"
)

const metricsGolden = "testdata/metrics.golden"

// TestMetricsPageMatchesGolden drives every family of the page runDaemon
// serves to a fixed state through the production registry and compares the
// page, minus the Go and process families whose values move, byte for byte.
// Not parallel: it swaps the package-level version.
func TestMetricsPageMatchesGolden(t *testing.T) {
	oldVersion := version
	version = "1.2.3"
	t.Cleanup(func() { version = oldVersion })

	at := func(hour, minute int) time.Time { return time.Date(2026, 3, 1, hour, minute, 0, 0, time.UTC) }
	completed := filepath.Join(t.TempDir(), "completed")
	seedStamp(t, completed, at(6, 0), "ok")
	reg, m := newMetricsRegistry("built-in", "app", scheduler.NewStamp(completed), 13*time.Hour)

	m.observe(scopeFull, runVerdict{result: resultAborted, exitCode: -1}, 270500*time.Millisecond, at(12, 0), time.Time{})
	m.observe(scopeScoped, runVerdict{result: resultComplete, ok: true}, 17*time.Minute, at(13, 0), time.Time{})
	m.observe(scopeScoped, runVerdict{result: resultRepositoryError, exitCode: 1, ok: true, tolerated: true},
		42250*time.Millisecond, at(14, 0), at(10, 30))
	m.rejectedRun(trigger.ErrFull)
	m.rejectedRun(trigger.ErrClosed)
	m.rejectedRun(trigger.ErrClosed)

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Content-Type"), "text/plain; version=0.0.4; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	got := []byte(withoutRuntimeFamilies(rec.Body.String()))

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(metricsGolden, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(metricsGolden)
	if err != nil {
		t.Fatalf("read %s: %v; write it with UPDATE_GOLDEN=1 go test -run TestMetricsPageMatchesGolden .", metricsGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the metrics page differs from %s; review the change, then rewrite it with UPDATE_GOLDEN=1 go test -run TestMetricsPageMatchesGolden .\ngot:\n%s\nwant:\n%s", metricsGolden, got, want)
	}
}

// withoutRuntimeFamilies drops every HELP, TYPE and sample line of the go_
// and process_ families from a text 0.0.4 page.
func withoutRuntimeFamilies(page string) string {
	var b strings.Builder
	for line := range strings.Lines(page) {
		name := line
		if rest, ok := strings.CutPrefix(line, "# HELP "); ok {
			name = rest
		} else if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name = rest
		}
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}
