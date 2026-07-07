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
)

// setupLogger installs a slog text handler that emits canonical logfmt
// (`time=... level=... msg=... k=v`) to stderr for Loki/Alloy collection.
// Renovate logs separately to stdout/stderr (set LOG_FORMAT=json); this
// logger covers only the scheduler's own lifecycle lines.
func setupLogger() {
	// slog.Level.UnmarshalText parses debug/info/warn/error case-insensitively
	// (and offset syntax such as "warn+1") but lacks the long-form "warning"
	// alias, so map it before parsing. An unrecognized value keeps the Info
	// default, matching the prior switch's fall-through.
	name := strings.ToLower(strings.TrimSpace(getEnv("LOG_LEVEL", "info")))
	if name == "warning" {
		name = "warn"
	}
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(name)); err != nil {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level, ReplaceAttr: utcTimeAttr})))
}

// utcTimeAttr is a slog ReplaceAttr that renders the record's built-in time
// key in UTC, so log-line timestamps are zone-stable regardless of the
// container's TZ (the fleet logs-in-UTC standard). It rewrites only the
// top-level time attribute; a user attribute that happens to share the "time"
// key inside a group is left untouched.
func utcTimeAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
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
// cadence and whether the built-in scheduler runs at all. SCHED_INTERVAL is
// a Go duration ("1h", "30m") that sets the interval. The sentinels "off"
// and "disabled" (case-insensitive) or any zero duration ("0", "0s")
// disable the built-in scheduler: the container idles and runs are
// triggered out-of-band via the `run` subcommand. Unset defaults to
// defaultInterval with the scheduler enabled. Any other parse failure falls
// back to defaultInterval and logs a warning rather than refusing to start.
func loadInterval() (interval time.Duration, scheduleEnabled bool) {
	interval = defaultInterval
	scheduleEnabled = true
	raw := strings.TrimSpace(os.Getenv("SCHED_INTERVAL"))
	if raw == "" {
		return interval, scheduleEnabled
	}
	switch strings.ToLower(raw) {
	case "off", "disabled":
		scheduleEnabled = false
	default:
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			slog.Warn("cannot parse SCHED_INTERVAL, using default",
				"value", raw, "default", defaultInterval)
		case d > 0:
			interval = d
		case d == 0:
			// Zero duration ("0", "0s") disables built-in scheduling.
			scheduleEnabled = false
		default:
			// A negative duration is not a valid interval and not a documented disable
			// sentinel (off/disabled/0/0s); warn and fall back to the default rather than
			// silently idling, which would mask a typo.
			slog.Warn("SCHED_INTERVAL is negative, using default",
				"value", raw, "default", defaultInterval)
		}
	}
	return interval, scheduleEnabled
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
