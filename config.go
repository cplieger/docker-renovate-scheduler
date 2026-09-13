// Package main implements docker-renovate-scheduler.
package main

import (
	"cmp"
	"context"
	"errors"
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

const (
	defaultInterval    = 6 * time.Hour
	defaultRunTimeout  = time.Hour
	baseDirProbeBudget = 10 * time.Second
	defaultBaseDir     = "/tmp/renovate"
	// The owner-only socket limits trigger authority to the container user.
	socketPath = "/tmp/docker-renovate-scheduler.sock"
	stampName  = ".docker-renovate-scheduler-last-run"
)

func setupLogger() {
	raw := envx.String("LOG_LEVEL")
	level, recognized := slogx.ParseLevel(raw, slog.LevelInfo)
	slogx.Setup(slogx.Options{Level: level})
	if !recognized {
		slog.Warn("unrecognized LOG_LEVEL, using default", "value", raw, "default", "info")
	}
}

func baseDir() string {
	return cmp.Or(envx.String("RENOVATE_BASE_DIR"), defaultBaseDir)
}

// stampPath derives from the daemon's boot-time base dir only; a per-run
// RENOVATE_BASE_DIR forwarded with a trigger never moves the stamp.
func stampPath() string {
	return filepath.Join(baseDir(), stampName)
}

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

func loadInterval() (interval time.Duration, scheduleEnabled bool) {
	s := scheduler.ParseInterval(os.Getenv("RUN_INTERVAL"), defaultInterval,
		scheduler.WithName("RUN_INTERVAL"))
	return s.Interval, s.Mode == scheduler.ModeBuiltin
}

func loadRunTimeout() time.Duration {
	d := envx.Duration("RUN_TIMEOUT", defaultRunTimeout)
	if d <= 0 {
		slog.Warn("RUN_TIMEOUT must be positive, using default",
			"value", d.String(), "default", defaultRunTimeout)
		return defaultRunTimeout
	}
	return d
}

func logBaseDirError(dir string, err error) {
	// A deadline means the probe never returned a verdict, so the volume is
	// not the thing to change.
	hint := "mount a writable volume at RENOVATE_BASE_DIR (the image default is /data); a read_only container needs a /data volume or tmpfs"
	if errors.Is(err, context.DeadlineExceeded) {
		hint = "the base directory did not answer within " + baseDirProbeBudget.String() +
			"; check that the volume backing RENOVATE_BASE_DIR is mounted and responding"
	}
	slog.Error("base directory preflight failed", "path", dir, "error", err, "hint", hint)
}

type baseDirVerifier struct {
	// Keep the slot until an uninterruptible filesystem probe returns.
	slot chan struct{}
}

func newBaseDirVerifier() *baseDirVerifier {
	return &baseDirVerifier{slot: make(chan struct{}, 1)}
}

func (v *baseDirVerifier) verify(ctx context.Context) error {
	return v.verifyAt(ctx, baseDir())
}

func (v *baseDirVerifier) verifyAt(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, baseDirProbeBudget)
	defer cancel()

	select {
	case v.slot <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("base dir verification timed out waiting for an earlier probe to finish: %w", ctx.Err())
	}

	done := make(chan error, 1)
	go func() {
		defer func() { <-v.slot }()
		done <- probeBaseDirWrite(ctx, dir)
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("base dir verification timed out: %w", ctx.Err())
	case err := <-done:
		return err
	}
}

func probeBaseDirWrite(ctx context.Context, dir string) error {
	res, err := atomicfile.ProbeWritable(ctx, dir, atomicfile.WithMkdirMode(0o700))
	if err != nil {
		return fmt.Errorf("base dir %q write probe not attempted: %w", dir, err)
	}
	return baseDirProbeResultError(dir, res)
}

func baseDirProbeResultError(dir string, res atomicfile.ProbeResult) error {
	if res.OK() {
		return nil
	}
	if res.Writable() {
		if res.Leaked {
			return fmt.Errorf("base dir %q probe %q failed to %s and leaked its file: %w", dir, res.Name, res.Stage, res.Err)
		}
		slog.Warn("base dir probe wrote and flushed but could not clean up",
			"path", dir, "stage", res.Stage, "name", res.Name,
			"leaked", res.Leaked, "error", res.Err)
		return nil
	}
	return fmt.Errorf("base dir %q not writable, failed to %s: %w", dir, res.Stage, res.Err)
}
