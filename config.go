// Package main implements docker-renovate-scheduler, a resident daemon that
// wraps self-hosted Renovate: PID 1 owns every run as a child process, driven
// by a built-in interval scheduler and/or a unix-socket trigger client.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/envx/v2"
	"github.com/cplieger/scheduler/v4"
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

	// socketPath is the daemon's trigger socket. The `run` subcommand dials
	// it to submit a run request; the daemon — the single owner of Renovate
	// execution — serves requests from its queue in order. /tmp is writable
	// by the image's non-root user and is per-container, and the socket file
	// is owner-only (0600), so trigger authority is scoped to the container's
	// own user — the same boundary `docker exec` already enforces.
	socketPath = "/tmp/docker-renovate-scheduler.sock"
)

// setupLogger installs a slog text handler that emits canonical logfmt
// (`time=... level=... msg=... k=v`) to stderr for Loki/Alloy collection.
// Renovate logs separately to stdout/stderr (set LOG_FORMAT=json); this
// logger covers only the scheduler's own lifecycle lines.
func setupLogger() {
	// An unrecognized value keeps the Info default, but is warned about —
	// after the fallback logger is installed, so the line reaches the real
	// handler — matching the interval/timeout parsers' fall-back diagnostics
	// (a typo like LOG_LEVEL=debig must not silently suppress the requested
	// visibility). Renovate itself also honours LOG_LEVEL.
	raw := strings.TrimSpace(envx.String("LOG_LEVEL", "info"))
	level, recognized := slogx.ParseLevel(raw, slog.LevelInfo)
	slogx.Setup(slogx.Options{Level: level})
	if !recognized {
		slog.Warn("unrecognized LOG_LEVEL, using default", "value", raw, "default", "info")
	}
}

// baseDir returns Renovate's base directory (RENOVATE_BASE_DIR), defaulting
// to defaultBaseDir. It is read — never set — by the scheduler so it can
// verify the directory is writable before handing off to Renovate.
func baseDir() string {
	return envx.String("RENOVATE_BASE_DIR", defaultBaseDir)
}

// baseDirForEnv resolves the base directory from the environment the child
// will actually receive: a job's forwarded environment (last value wins for
// duplicate keys, matching exec semantics), or the daemon's own environment
// when env is nil (ticker-submitted runs). This keeps the per-run preflight
// validating the same directory Renovate will use, honouring the documented
// complete RENOVATE_* passthrough.
func baseDirForEnv(env []string) string {
	if env == nil {
		return baseDir()
	}
	for _, kv := range slices.Backward(env) {
		key, value, ok := strings.Cut(kv, "=")
		if ok && key == "RENOVATE_BASE_DIR" {
			if value == "" {
				return defaultBaseDir
			}
			return value
		}
	}
	return defaultBaseDir
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
// defaultRunTimeout on unset or unparseable values (envx.Duration warns on
// malformed input), logging a warning rather than refusing to start. The
// positive-only rule stays app-side: a zero or negative timeout would expire
// every run before it starts.
func loadRunTimeout() time.Duration {
	d := envx.Duration("SCHED_TIMEOUT", defaultRunTimeout)
	if d <= 0 {
		slog.Warn("SCHED_TIMEOUT must be positive, using default",
			"value", d.String(), "default", defaultRunTimeout)
		return defaultRunTimeout
	}
	return d
}

// logBaseDirError reports the "base directory not writable" failure with its
// remediation hint, so the boot-time and per-run checks cannot drift.
func logBaseDirError(dir string, err error) {
	slog.Error("base directory not writable", "path", dir, "error", err,
		"hint", "mount a writable volume at RENOVATE_BASE_DIR (the image default is /data); a read_only container needs a /data volume or tmpfs")
}

// verifyBaseDir creates Renovate's base directory if missing and confirms a
// file can be written inside it, failing fast with an actionable hint when
// the filesystem is read-only or the volume is unwritable (the common
// misconfiguration: a read_only container without a writable volume or
// tmpfs at RENOVATE_BASE_DIR). Checked at daemon boot and again before each
// run, so a volume that degrades after boot fails loudly instead of deep
// inside Renovate.
func verifyBaseDir(ctx context.Context) error {
	return verifyBaseDirAt(ctx, baseDir())
}

// verifySlot is a process-wide one-slot semaphore serializing base-dir
// verification probes. The 10-second context bounds only the caller's wait,
// not the filesystem syscalls in the probe goroutine: on a hung bind/network
// filesystem the goroutine stays blocked after the caller returns. The slot
// is released only when that goroutine actually finishes, so later calls
// time out waiting for the slot instead of stacking another blocked
// goroutine (and potentially an OS thread) per probe.
var verifySlot = make(chan struct{}, 1)

// verifyBaseDirAt is verifyBaseDir against an explicit directory — the
// per-run form, so a job's forwarded RENOVATE_BASE_DIR is validated instead
// of the daemon's own.
func verifyBaseDirAt(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	select {
	case verifySlot <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("base dir verification timed out: %w", ctx.Err())
	}

	done := make(chan error, 1)
	go func() {
		defer func() { <-verifySlot }()
		done <- probeBaseDirWrite(ctx, dir)
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("base dir verification timed out: %w", ctx.Err())
	case err := <-done:
		return err
	}
}

// probeBaseDirWrite proves the base directory is genuinely writable: create
// it if missing, then create a unique probe file, write and sync one byte,
// and close and remove it — every stage checked. A filesystem can accept the
// directory entry yet reject the first data write, surface a delayed failure
// only at Sync/Close, or deny cleanup; each of those breaks the preflight
// promise ("Renovate can write here"), so each fails the probe.
//
// The ladder itself is atomicfile.ProbeWritable, whose ProbeStage set covers
// every stage this probe distinguishes (mkdir, create, write, sync, close,
// remove) with the same first-failure-wins precedence the hand-rolled probe
// had; baseDirProbeStageError keeps each stage's error worded as before. The
// probe file also carries atomicfile's own temp-name shape instead of an
// app-invented ".write_test-*", so a leftover is recognisable as a package
// temp rather than as a name nothing in the fleet knows (nothing in this repo
// sweeps RENOVATE_BASE_DIR — see the note on baseDirProbeStageError's remove
// stage).
//
// ctx bounds only whether the probe is ATTEMPTED: atomicfile checks it once,
// before creating anything, and the stages themselves are uninterruptible
// filesystem calls — which is why verifyBaseDirAt still needs its own slot and
// timeout around this call.
func probeBaseDirWrite(ctx context.Context, dir string) error {
	res, err := atomicfile.ProbeWritable(ctx, dir, atomicfile.WithMkdirMode(0o700))
	if err != nil {
		// A non-nil error means the probe was never attempted (an empty dir, a
		// context already done). It carries NO verdict on writability, so it
		// must not be reported with any of the stage diagnoses below — a
		// cancelled preflight is not a read-only volume.
		return fmt.Errorf("base dir %q write probe not attempted: %w", dir, err)
	}
	return baseDirProbeStageError(dir, res)
}

// baseDirProbeStageError renders one probe outcome as this app's error for the
// stage that failed, so every stage stays distinguishable in the log exactly as
// it was when the probe was hand-rolled: the create failure keeps the
// actionable "not writable" wording logBaseDirError's hint is written against,
// and each later stage names itself and the probe file.
//
// Every stage is fatal, unchanged: the preflight promise is that Renovate can
// write in this directory AND clean up after itself, and a directory that
// denies the unlink accumulates Renovate's own scratch until the volume fills.
// That also makes the leak reclaimability question moot in practice here — the
// only way this probe leaves a file behind is a denied unlink, which a sweep
// could not remove either.
func baseDirProbeStageError(dir string, res atomicfile.ProbeResult) error {
	if res.OK() {
		return nil
	}
	probe := filepath.Join(res.Dir, res.Name)
	switch res.Stage {
	case atomicfile.ProbeStageMkdir:
		return fmt.Errorf("mkdir base dir %q: %w", dir, res.Err)
	case atomicfile.ProbeStageCreate:
		return fmt.Errorf("base dir %q not writable: %w", dir, res.Err)
	case atomicfile.ProbeStageWrite:
		return fmt.Errorf("write base-dir probe %q: %w", probe, res.Err)
	case atomicfile.ProbeStageSync:
		return fmt.Errorf("sync base-dir probe %q: %w", probe, res.Err)
	case atomicfile.ProbeStageClose:
		return fmt.Errorf("close base-dir probe %q: %w", probe, res.Err)
	case atomicfile.ProbeStageRemove:
		return fmt.Errorf("remove base-dir probe %q: %w", probe, res.Err)
	default:
		// Unreachable today (ProbeStageNone is handled by res.OK() above), and
		// deliberately not a silent nil: a stage the library adds later must
		// surface as a failure rather than as a passing preflight.
		return fmt.Errorf("base dir %q write probe failed at %s: %w", dir, res.Stage, res.Err)
	}
}
