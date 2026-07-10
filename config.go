// Package main implements docker-renovate-scheduler, a resident daemon that
// wraps self-hosted Renovate with an interval scheduler and advisory overlap guard.
package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cplieger/scheduler"
	"github.com/cplieger/slogx"
)

// --- Configuration ---
//
// This scheduler has no config file of its own: Renovate reads its entire
// configuration from its own RENOVATE_* environment variables, a config.js,
// or a config file (see config.js.example). The scheduler is configured by
// three environment variables, all deliberately OUTSIDE Renovate's
// RENOVATE_* namespace so Renovate cannot misinterpret them as config
// options:
//
//	SCHED_INTERVAL  built-in cadence (Go duration), or off/disabled/0 to
//	                disable the built-in scheduler and trigger runs externally
//	SCHED_TIMEOUT   whole-run timeout for a single `renovate` invocation
//	LOG_LEVEL       slog level (also honoured by Renovate itself)

const (
	// defaultInterval is the fallback built-in scheduler cadence when
	// SCHED_INTERVAL is unset or unparseable (non-sentinel). Six hours keeps
	// dependencies fresh without hammering registries or platform APIs.
	defaultInterval = 6 * time.Hour

	// defaultRunTimeout caps a single `renovate` invocation — the whole run
	// across every configured repository. Override with SCHED_TIMEOUT.
	// Renovate's own RENOVATE_EXECUTION_TIMEOUT is a separate per-child-
	// process limit inside Renovate; this is the outer bound on the process.
	defaultRunTimeout = time.Hour

	// defaultBaseDir is where Renovate stores repo clones and caches when
	// RENOVATE_BASE_DIR is unset. The Docker image sets RENOVATE_BASE_DIR to
	// a persisted volume; this matches Renovate's own default otherwise.
	defaultBaseDir = "/tmp/renovate"

	// lockFilePath guards against overlapping runs. flock(2) on this file
	// serialises runs both in-process (the built-in ticker racing the
	// startup run) and cross-process (an external `run` invocation racing
	// the built-in ticker or a manual docker exec — e.g. an Ofelia tick
	// racing an externally-triggered exec). Renovate is not safe to run
	// concurrently against the same repositories and base dir. /tmp is
	// writable by Renovate's non-root user, same place as the health marker.
	lockFilePath = "/tmp/.docker-renovate-scheduler.lock"

	// rerunFlagPath is the single-slot coalescing flag for the overlap guard.
	// When a trigger arrives while lockFilePath is held, the loser touches
	// this file instead of dropping the request; the active holder reruns once
	// on completion if it is set. Any number of overlapping triggers collapse
	// into exactly one queued rerun ("max 1 wait"). Sibling of the lock file
	// in the same Renovate-writable /tmp.
	rerunFlagPath = "/tmp/.docker-renovate-scheduler.rerun"

	// drainMarkerPath is the daemon->exec-child shutdown latch. On SIGTERM the
	// daemon (PID 1) sets it; an in-flight external `run` process checks it
	// before launching each coalesced pass and stops (drains) once it is set.
	// It exists because `docker stop` delivers SIGTERM only to PID 1, never to
	// the separate `docker exec` run process, so the child cannot observe the
	// container's shutdown through its own signal context — this marker is the
	// only channel by which the daemon can tell it to stop starting new passes
	// and drain within stop_grace_period. Sibling of the lock and rerun flag in
	// the same Renovate-writable /tmp; /tmp is per-container so it never
	// survives a recreate (no cross-run staleness).
	drainMarkerPath = "/tmp/.docker-renovate-scheduler.draining"
)

// setupLogger installs a slog text handler that emits canonical logfmt
// (`time=... level=... msg=... k=v`) to stderr for Loki/Alloy collection.
// Renovate logs separately to stdout/stderr (set LOG_FORMAT=json); this
// logger covers only the scheduler's own lifecycle lines.
func setupLogger() {
	// An unrecognized value keeps the Info default (silent, matching the prior
	// switch's fall-through); Renovate itself also honours LOG_LEVEL.
	level, _ := slogx.ParseLevel(getEnv("LOG_LEVEL", "info"), slog.LevelInfo)
	slogx.Setup(slogx.Options{Level: level})
}

// getEnv returns the environment value for key, or fallback when unset or
// empty.
func getEnv(key, fallback string) string {
	return cmp.Or(os.Getenv(key), fallback)
}

// baseDir returns Renovate's base directory (RENOVATE_BASE_DIR), defaulting
// to defaultBaseDir. It is read — never set — by the scheduler so it can
// verify the directory is writable before handing off to Renovate.
func baseDir() string {
	return getEnv("RENOVATE_BASE_DIR", defaultBaseDir)
}

// loadInterval parses SCHED_INTERVAL and reports the built-in scheduler
// cadence and whether the built-in scheduler runs at all. It delegates to
// scheduler.ParseInterval, the fleet-standard *_INTERVAL parser: a Go
// duration ("1h", "30m") sets the interval; the sentinels "off"/"disabled"
// (case-insensitive) or a zero duration ("0", "0s") select external mode
// (the built-in scheduler idles and runs are triggered out-of-band via the
// `run` subcommand); unset, negative, or unparseable falls back to
// defaultInterval with the scheduler enabled (a warning is logged for the
// negative and unparseable cases). scheduleEnabled is true only in built-in
// mode.
func loadInterval() (interval time.Duration, scheduleEnabled bool) {
	s := scheduler.ParseInterval(os.Getenv("SCHED_INTERVAL"), defaultInterval,
		scheduler.WithName("SCHED_INTERVAL"))
	return s.Interval, s.Mode == scheduler.ModeBuiltin
}

// loadRunTimeout reads SCHED_TIMEOUT (a Go duration) and falls back to
// defaultRunTimeout on unset or unparseable values, logging a warning
// rather than refusing to start.
func loadRunTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SCHED_TIMEOUT"))
	if raw == "" {
		return defaultRunTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("cannot parse SCHED_TIMEOUT, using default",
			"value", raw, "default", defaultRunTimeout)
		return defaultRunTimeout
	}
	return d
}

// verifyBaseDir creates Renovate's base directory if missing and confirms a
// file can be written inside it, failing fast with an actionable hint when
// the filesystem is read-only or the volume is unwritable (the common
// misconfiguration: a read_only container without a writable volume or
// tmpfs at RENOVATE_BASE_DIR).
func verifyBaseDir(ctx context.Context) error {
	dir := baseDir()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			done <- fmt.Errorf("mkdir base dir %q: %w", dir, err)
			return
		}
		testFile := filepath.Join(dir, ".write_test")
		f, err := os.Create(testFile) // #nosec G304 -- fixed filename in the operator-provided base dir
		if err != nil {
			done <- fmt.Errorf("base dir %q not writable: %w", dir, err)
			return
		}
		_ = f.Close()
		_ = os.Remove(testFile)
		done <- nil
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("base dir verification timed out: %w", ctx.Err())
	case err := <-done:
		return err
	}
}
