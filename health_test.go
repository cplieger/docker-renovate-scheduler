package main

import (
	"strconv"
	"testing"
	"time"
)

// TestHealthMarkerPath pins the file the image's healthcheck reads: in
// /dev/shm, which stays writable under a read-only root with no extra mount.
func TestHealthMarkerPath(t *testing.T) {
	if want := "/dev/shm/docker-renovate-scheduler.healthy"; healthMarkerPath != want {
		t.Errorf("healthMarkerPath = %q, want %q", healthMarkerPath, want)
	}
}

// TestProbeLease pins the freshness deadline the health subcommand arms: two
// run intervals plus one RUN_TIMEOUT, plus the token request's budget in
// GitHub App mode, and none in external mode. Serial: it sets the environment.
func TestProbeLease(t *testing.T) {
	for _, tc := range []struct {
		name, interval, timeout string
		app                     bool
		want                    time.Duration
	}{
		{name: "built-in defaults", want: 2*6*time.Hour + time.Hour},
		{name: "two intervals and one run timeout", interval: "1m", timeout: "10s", want: 2*time.Minute + 10*time.Second},
		{name: "GitHub App mode adds the token request", interval: "1h", timeout: "30m", app: true, want: 2*time.Hour + 30*time.Minute + 2*time.Minute},
		{name: "external mode arms none", interval: "off"},
		{name: "external mode in GitHub App mode arms none", interval: "off", app: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAppEnv(t)
			t.Setenv("RUN_INTERVAL", tc.interval)
			t.Setenv("RUN_TIMEOUT", tc.timeout)
			if tc.app {
				t.Setenv(envAppID, strconv.Itoa(testAppID))
				t.Setenv(envAppKeyFile, writeKeyFile(t, false))
			}
			if got := probeLease(); got != tc.want {
				t.Errorf("probeLease(RUN_INTERVAL=%q, RUN_TIMEOUT=%q, app=%v) = %v, want %v", tc.interval, tc.timeout, tc.app, got, tc.want)
			}
		})
	}
}

// TestProbeLeaseStaysArmedAtTheDurationLimit: RUN_INTERVAL and RUN_TIMEOUT
// have no ceiling, so their sum can pass int64. A wrapped negative lease reads
// as none and would disarm the stuck-schedule check, so it saturates instead.
// Serial: it sets the environment.
func TestProbeLeaseStaysArmedAtTheDurationLimit(t *testing.T) {
	for _, tc := range []struct {
		name, interval, timeout string
		app                     bool
		atLeast                 time.Duration
	}{
		{name: "an interval whose double passes int64", interval: "1500000h", timeout: "1s", atLeast: 1500000 * time.Hour},
		{name: "a timeout near int64 plus the App mode token budget", interval: "1h", timeout: "2562047h47m", app: true, atLeast: 2562047*time.Hour + 47*time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAppEnv(t)
			t.Setenv("RUN_INTERVAL", tc.interval)
			t.Setenv("RUN_TIMEOUT", tc.timeout)
			if tc.app {
				t.Setenv(envAppID, strconv.Itoa(testAppID))
				t.Setenv(envAppKeyFile, writeKeyFile(t, false))
			}
			if got := probeLease(); got < tc.atLeast {
				t.Errorf("probeLease() = %v, want at least %v", got, tc.atLeast)
			}
		})
	}
}
