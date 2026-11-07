package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/measure"
)

// healthzBody is the JSON body every cplieger /healthz answers with.
type healthzBody struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}

// setMarker puts a test marker in a state: written by a good or tolerated run
// (fresh), removed by a failed one (failed), or written age ago (aged).
func setMarker(t *testing.T, marker *health.Marker, path, state string, age time.Duration) {
	t.Helper()
	switch state {
	case "fresh":
		marker.Set(true)
	case "failed":
		marker.Set(false)
	case "aged":
		marker.Set(true)
		aged := time.Now().Add(-age)
		if err := os.Chtimes(path, aged, aged); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown marker state %q", state)
	}
}

func getHealthz(t *testing.T, client *http.Client, url string) (int, http.Header, healthzBody) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body healthzBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("GET /healthz body %q is not JSON: %v", raw, err)
	}
	return resp.StatusCode, resp.Header, body
}

// TestServeMetricsServesHealthz pins the route on METRICS_LISTEN_ADDR: GET and
// HEAD answer 200 {"status":"ok"} on a fresh marker, 503 with one reason per
// fault on a failed or a stale marker, and any other method gets 405.
func TestServeMetricsServesHealthz(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")
	marker := health.NewMarker(path)
	// A 1h interval and a 30m run timeout: a lease of two intervals plus one timeout.
	lease := runLease(time.Hour, true, 30*time.Minute)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	stop, err := serveMetrics(ctx, addr, measure.NewRegistry(metricsPrefix), newHealthz(marker, lease))
	if err != nil {
		cancel()
		t.Fatalf("serveMetrics(%q) error = %v", addr, err)
	}
	t.Cleanup(func() {
		cancel()
		stop()
	})
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://" + addr + "/healthz"

	const pastLease = 3 * time.Hour
	reasons := map[string]string{}
	for _, tc := range []struct {
		name       string
		state      string
		wantStatus string
		wantCode   int
	}{
		{name: "fresh", state: "fresh", wantCode: http.StatusOK, wantStatus: "ok"},
		{name: "failed", state: "failed", wantCode: http.StatusServiceUnavailable, wantStatus: "unhealthy"},
		{name: "stale", state: "aged", wantCode: http.StatusServiceUnavailable, wantStatus: "unhealthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setMarker(t, marker, path, tc.state, pastLease)
			code, header, body := getHealthz(t, client, url)
			if code != tc.wantCode || body.Status != tc.wantStatus {
				t.Errorf("GET /healthz on a %s marker = %d %q, want %d %q", tc.name, code, body.Status, tc.wantCode, tc.wantStatus)
			}
			for key, want := range map[string]string{
				"Content-Type":           "application/json",
				"Cache-Control":          "no-store",
				"X-Content-Type-Options": "nosniff",
			} {
				if got := header.Get(key); got != want {
					t.Errorf("GET /healthz %s = %q, want %q", key, got, want)
				}
			}
			switch {
			case tc.wantCode == http.StatusOK && body.Reasons != nil:
				t.Errorf("GET /healthz on a fresh marker lists reasons %q, want none", body.Reasons)
			case tc.wantCode != http.StatusOK && len(body.Reasons) != 1:
				t.Errorf("GET /healthz on a %s marker lists reasons %q, want exactly one", tc.name, body.Reasons)
			case tc.wantCode != http.StatusOK:
				reasons[tc.name] = body.Reasons[0]
			}
		})
	}
	if reasons["failed"] == reasons["stale"] {
		t.Errorf("a failed and a stale marker both report %q, want a reason naming each fault", reasons["failed"])
	}

	t.Run("HEAD", func(t *testing.T) {
		setMarker(t, marker, path, "fresh", 0)
		resp, err := client.Head(url)
		if err != nil {
			t.Fatalf("HEAD /healthz: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("HEAD /healthz = %d, want 200", resp.StatusCode)
		}
	})
	t.Run("POST", func(t *testing.T) {
		resp, err := client.Post(url, "application/json", nil)
		if err != nil {
			t.Fatalf("POST /healthz: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST /healthz = %d, want 405", resp.StatusCode)
		}
	})
}

// TestHealthzAgreesWithTheHealthSubcommand pins one verdict for two readers:
// for a fresh, a failed, a not yet stale and a stale marker, /healthz answers
// 200 exactly when the health subcommand exits 0. With the schedule on, the
// lease is two run intervals plus one run timeout; with RUN_INTERVAL=off there
// is none. Serial: it sets the environment both readers parse.
func TestHealthzAgreesWithTheHealthSubcommand(t *testing.T) {
	for _, env := range []struct {
		interval, timeout string
		lease             time.Duration
	}{
		{interval: "1h", timeout: "30m", lease: 2*time.Hour + 30*time.Minute},
		{interval: "", timeout: "", lease: 2*6*time.Hour + time.Hour},
		{interval: "1h", timeout: "0", lease: 2*time.Hour + time.Hour},
		{interval: "off", timeout: ""},
	} {
		// With no lease, any age is healthy; 100 hours stands for "long ago".
		within, past := env.lease-time.Hour, env.lease+time.Hour
		pastCode := http.StatusServiceUnavailable
		if env.lease == 0 {
			within, past = time.Hour, 100*time.Hour
			pastCode = http.StatusOK
		}
		for _, tc := range []struct {
			state    string
			age      time.Duration
			wantCode int
		}{
			{state: "fresh", wantCode: http.StatusOK},
			{state: "failed", wantCode: http.StatusServiceUnavailable},
			{state: "aged", age: within, wantCode: http.StatusOK},
			{state: "aged", age: past, wantCode: pastCode},
		} {
			name := "RUN_INTERVAL=" + env.interval + " RUN_TIMEOUT=" + env.timeout + " " + tc.state + " " + tc.age.String()
			t.Run(name, func(t *testing.T) {
				t.Setenv("RUN_INTERVAL", env.interval)
				t.Setenv("RUN_TIMEOUT", env.timeout)
				interval, scheduleEnabled := loadInterval()
				path := filepath.Join(t.TempDir(), "healthy")
				marker := health.NewMarker(path)
				setMarker(t, marker, path, tc.state, tc.age)

				rec := httptest.NewRecorder()
				newHealthz(marker, runLease(interval, scheduleEnabled, loadRunTimeout())).
					ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
				probe := health.ProbeCheck(path, probeOptions()...)
				if rec.Code != tc.wantCode {
					t.Errorf("/healthz = %d, want %d", rec.Code, tc.wantCode)
				}
				if (probe == 0) != (rec.Code == http.StatusOK) {
					t.Errorf("/healthz = %d but the health subcommand exits %d: the two readers disagree", rec.Code, probe)
				}
			})
		}
	}
}

// TestDaemonServesHealthzOnTheMetricsAddress: the daemon wires /healthz to its
// own marker, so a booted external-mode daemon answers 200, and unhealthy
// once its health latch drops. Not parallel: it uses the package-global
// health marker path.
func TestDaemonServesHealthzOnTheMetricsAddress(t *testing.T) {
	addr := freeAddr(t)
	daemonEnv(t, addr)
	startDaemon(t)
	getMetrics(t, addr)
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://" + addr + "/healthz"

	if code, _, body := getHealthz(t, client, url); code != http.StatusOK || body.Status != "ok" {
		t.Errorf("GET /healthz on a booted daemon = %d %q, want 200 \"ok\"", code, body.Status)
	}
	health.NewMarker(healthMarkerPath).Set(false)
	if code, _, body := getHealthz(t, client, url); code != http.StatusServiceUnavailable || len(body.Reasons) != 1 {
		t.Errorf("GET /healthz after a failed run = %d %q, want 503 with one reason", code, body.Reasons)
	}
}
