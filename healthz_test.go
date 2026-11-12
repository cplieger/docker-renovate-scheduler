package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/measure"
)

// healthzBody is the JSON body every cplieger /healthz answers with.
type healthzBody struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
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

// serveHealthz answers one GET /healthz from h and decodes its body.
func serveHealthz(t *testing.T, h http.Handler) (int, healthzBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body healthzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /healthz body %q is not JSON: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

// TestServeMetricsServesHealthz pins the route on METRICS_LISTEN_ADDR: GET and
// HEAD answer 200 {"status":"ok"} after a good run, 503 with one reason after
// a failed one, and any other method gets 405.
func TestServeMetricsServesHealthz(t *testing.T) {
	st := health.NewStatus()
	lease := runLease(time.Hour, true, 30*time.Minute, false)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	stop, err := serveMetrics(ctx, addr, measure.NewRegistry(metricsPrefix), newHealthz(st, lease))
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

	for _, tc := range []struct {
		name       string
		wantStatus string
		wantCode   int
		healthy    bool
	}{
		{name: "fresh", healthy: true, wantCode: http.StatusOK, wantStatus: "ok"},
		{name: "failed", wantCode: http.StatusServiceUnavailable, wantStatus: "unhealthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st.Set(tc.healthy)
			code, header, body := getHealthz(t, client, url)
			if code != tc.wantCode || body.Status != tc.wantStatus {
				t.Errorf("GET /healthz %s = %d %q, want %d %q", tc.name, code, body.Status, tc.wantCode, tc.wantStatus)
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
			if tc.healthy && body.Reasons != nil {
				t.Errorf("GET /healthz after a good run lists reasons %q, want none", body.Reasons)
			}
			if !tc.healthy && (len(body.Reasons) != 1 || body.Reasons[0] != reasonRunFailed) {
				t.Errorf("GET /healthz after a failed run lists reasons %q, want only %q", body.Reasons, reasonRunFailed)
			}
		})
	}

	t.Run("HEAD", func(t *testing.T) {
		st.Set(true)
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

// TestHealthzNamesEachFault pins one reason per fault: a failed run reports
// reasonRunFailed alone, and a good run older than the lease reports
// reasonStuck alone.
func TestHealthzNamesEachFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := health.NewStatus()
		// A 1h interval and a 30m run timeout: a lease of two intervals plus one timeout.
		h := newHealthz(st, 2*time.Hour+30*time.Minute)

		st.Set(false)
		if code, body := serveHealthz(t, h); code != http.StatusServiceUnavailable || len(body.Reasons) != 1 || body.Reasons[0] != reasonRunFailed {
			t.Errorf("GET /healthz after a failed run = %d %q, want 503 with only %q", code, body.Reasons, reasonRunFailed)
		}
		st.Set(true)
		time.Sleep(2*time.Hour + 30*time.Minute + time.Second)
		if code, body := serveHealthz(t, h); code != http.StatusServiceUnavailable || len(body.Reasons) != 1 || body.Reasons[0] != reasonStuck {
			t.Errorf("GET /healthz past the lease = %d %q, want 503 with only %q", code, body.Reasons, reasonStuck)
		}
	})
}

// TestTheLeaseInAppModeCoversTheTokenRequest pins the lease to one run of the
// work loop: in GitHub App mode a run may spend tokenMintBudget on its token
// before RUN_TIMEOUT starts, so a good run that long ago is still healthy
// there, while in token mode, whose runs ask for no token, it is stuck. The
// health subcommand arms the same lease in both modes, whether or not the key
// file is still there. Serial: it sets the environment.
func TestTheLeaseInAppModeCoversTheTokenRequest(t *testing.T) {
	const interval, runTimeout = time.Minute, 30 * time.Minute
	// A run that starts as the second interval ends, spends the whole token
	// budget and then most of RUN_TIMEOUT.
	const age = 2*interval + 2*time.Minute + runTimeout - 30*time.Second
	for _, tc := range []struct {
		name     string
		app      bool
		keyGone  bool
		wantCode int
	}{
		{name: "GitHub App mode", app: true, wantCode: http.StatusOK},
		{name: "GitHub App mode after the key file is gone", app: true, keyGone: true, wantCode: http.StatusOK},
		{name: "token mode", wantCode: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUN_INTERVAL", interval.String())
			t.Setenv("RUN_TIMEOUT", runTimeout.String())
			clearAppEnv(t)
			if tc.app {
				key := writeKeyFile(t, false)
				if tc.keyGone {
					if err := os.Remove(key); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv(envAppID, strconv.Itoa(testAppID))
				t.Setenv(envAppKeyFile, key)
			}
			lease := runLease(interval, true, runTimeout, tc.app)
			if got := probeLease(); got != lease {
				t.Errorf("the health subcommand arms %v, /healthz %v: the two readers disagree", got, lease)
			}
			synctest.Test(t, func(t *testing.T) {
				st := health.NewStatus()
				h := newHealthz(st, lease)
				st.Set(true)
				time.Sleep(age)
				if code, _ := serveHealthz(t, h); code != tc.wantCode {
					t.Errorf("/healthz %s after a good run = %d, want %d", age, code, tc.wantCode)
				}
			})
		})
	}
}

// TestHealthzAgreesWithTheHealthSubcommand pins one deadline for two readers:
// for each schedule the lease the health subcommand arms is the one /healthz
// applies, and /healthz turns 503 just past it after a good run (never, with
// RUN_INTERVAL=off). Serial: it sets the environment both readers parse.
func TestHealthzAgreesWithTheHealthSubcommand(t *testing.T) {
	clearAppEnv(t)
	for _, env := range []struct {
		interval, timeout string
		lease             time.Duration
	}{
		{interval: "1h", timeout: "30m", lease: 2*time.Hour + 30*time.Minute},
		{interval: "", timeout: "", lease: 2*6*time.Hour + time.Hour},
		{interval: "1h", timeout: "0", lease: 2*time.Hour + time.Hour},
		{interval: "off", timeout: ""},
	} {
		t.Run("RUN_INTERVAL="+env.interval+" RUN_TIMEOUT="+env.timeout, func(t *testing.T) {
			t.Setenv("RUN_INTERVAL", env.interval)
			t.Setenv("RUN_TIMEOUT", env.timeout)
			if got := probeLease(); got != env.lease {
				t.Fatalf("the health subcommand arms %v, want %v", got, env.lease)
			}
			interval, scheduleEnabled := loadInterval()
			lease := runLease(interval, scheduleEnabled, loadRunTimeout(), false)
			synctest.Test(t, func(t *testing.T) {
				st := health.NewStatus()
				h := newHealthz(st, lease)
				st.Set(true)
				// With no lease, any age is healthy; 100 hours stands for "long ago".
				within, past, pastCode := env.lease-time.Second, 2*time.Second, http.StatusServiceUnavailable
				if env.lease == 0 {
					within, past, pastCode = time.Hour, 100*time.Hour, http.StatusOK
				}
				time.Sleep(within)
				if code, _ := serveHealthz(t, h); code != http.StatusOK {
					t.Errorf("/healthz %v after a good run = %d, want 200", within, code)
				}
				time.Sleep(past)
				if code, _ := serveHealthz(t, h); code != pastCode {
					t.Errorf("/healthz past the lease = %d, want %d", code, pastCode)
				}
			})
		})
	}
}

// TestDaemonServesHealthzOnTheMetricsAddress: the daemon wires /healthz to its
// own verdict, so a booted external-mode daemon answers 200, and 503 naming
// the failed run once a triggered run fails. Not parallel: it uses the
// package-global health marker path.
func TestDaemonServesHealthzOnTheMetricsAddress(t *testing.T) {
	addr := freeAddr(t)
	daemonEnv(t, addr)
	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, sock, recordingRunner("false", nil)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("runDaemon() = %v, want nil", err)
		}
	})
	getMetrics(t, addr)
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://" + addr + "/healthz"

	// External mode boots healthy once the trigger server starts, after the
	// metrics listener binds.
	waitFor(t, 5*time.Second, func() bool {
		code, _, body := getHealthz(t, client, url)
		return code == http.StatusOK && body.Status == "ok"
	}, "GET /healthz on a booted external-mode daemon never answered 200 \"ok\"")
	if code := runClient(sock, nil); code != 1 {
		t.Fatalf("runClient() = %d, want 1 (the run fails)", code)
	}
	if code, _, body := getHealthz(t, client, url); code != http.StatusServiceUnavailable || len(body.Reasons) != 1 || body.Reasons[0] != reasonRunFailed {
		t.Errorf("GET /healthz after a failed run = %d %q, want 503 with only %q", code, body.Reasons, reasonRunFailed)
	}
}
