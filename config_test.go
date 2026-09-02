package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/atomicfile/v3"
)

func TestLoadInterval(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		wantInterval time.Duration
		wantEnabled  bool
	}{
		{"duration", "30m", 30 * time.Minute, true},
		{"hour duration", "1h", time.Hour, true},
		{"off", "off", defaultInterval, false},
		{"off uppercase", "OFF", defaultInterval, false},
		{"disabled", "disabled", defaultInterval, false},
		{"disabled mixed case", "Disabled", defaultInterval, false},
		{"zero", "0", defaultInterval, false},
		{"zero seconds", "0s", defaultInterval, false},
		{"unset defaults to enabled", "", defaultInterval, true},
		{"unparseable falls back enabled", "not-a-duration", defaultInterval, true},
		{"negative duration falls back enabled, not disabled", "-5m", defaultInterval, true},
		{"whitespace trimmed", "  off  ", defaultInterval, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RUN_INTERVAL", tt.env)
			interval, enabled := loadInterval()
			if enabled != tt.wantEnabled {
				t.Errorf("loadInterval() enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if interval != tt.wantInterval {
				t.Errorf("loadInterval() interval = %v, want %v", interval, tt.wantInterval)
			}
		})
	}
}

func TestLoadRunTimeout(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("RUN_TIMEOUT", "")
		if got := loadRunTimeout(); got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() = %v, want %v", got, defaultRunTimeout)
		}
	})
	t.Run("parsed value", func(t *testing.T) {
		t.Setenv("RUN_TIMEOUT", "30m")
		if got := loadRunTimeout(); got != 30*time.Minute {
			t.Errorf("loadRunTimeout() = %v, want 30m", got)
		}
	})
	t.Run("default on garbage", func(t *testing.T) {
		t.Setenv("RUN_TIMEOUT", "not-a-duration")
		if got := loadRunTimeout(); got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() = %v, want %v", got, defaultRunTimeout)
		}
	})
	t.Run("default on non-positive", func(t *testing.T) {
		t.Setenv("RUN_TIMEOUT", "-5m")
		if got := loadRunTimeout(); got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() = %v, want %v", got, defaultRunTimeout)
		}
	})
}

func TestBaseDir(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("RENOVATE_BASE_DIR", "")
		if got := baseDir(); got != defaultBaseDir {
			t.Errorf("baseDir() = %q, want %q", got, defaultBaseDir)
		}
	})
	t.Run("honours env", func(t *testing.T) {
		t.Setenv("RENOVATE_BASE_DIR", "/data")
		if got := baseDir(); got != "/data" {
			t.Errorf("baseDir() = %q, want /data", got)
		}
	})
}

func TestBaseDirForEnv(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"nil env uses the daemon's own base dir", nil, "/daemon-dir"},
		{"forwarded env with the key uses its value", []string{"RENOVATE_BASE_DIR=/from-client"}, "/from-client"},
		{"forwarded env without the key uses the default, not the daemon's", []string{"PATH=/usr/bin"}, defaultBaseDir},
		{"empty value falls back to the default", []string{"RENOVATE_BASE_DIR="}, defaultBaseDir},
		{"duplicate keys: last value wins (exec semantics)", []string{"RENOVATE_BASE_DIR=/first", "RENOVATE_BASE_DIR=/second"}, "/second"},
		{"malformed entry without '=' is skipped", []string{"RENOVATE_BASE_DIR", "PATH=/usr/bin"}, defaultBaseDir},
		{"empty non-nil env uses the default", []string{}, defaultBaseDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", "/daemon-dir")
			if got := baseDirForEnv(tt.env); got != tt.want {
				t.Errorf("baseDirForEnv(%v) = %q, want %q", tt.env, got, tt.want)
			}
		})
	}
}

func TestStampPath(t *testing.T) {
	t.Run("derives from the default base dir when unset", func(t *testing.T) {
		t.Setenv("RENOVATE_BASE_DIR", "")
		if got, want := stampPath(), filepath.Join(defaultBaseDir, stampName); got != want {
			t.Errorf("stampPath() = %q, want %q", got, want)
		}
	})
	t.Run("follows the daemon's RENOVATE_BASE_DIR", func(t *testing.T) {
		t.Setenv("RENOVATE_BASE_DIR", "/data")
		if got, want := stampPath(), filepath.Join("/data", stampName); got != want {
			t.Errorf("stampPath() = %q, want %q", got, want)
		}
	})
}

func TestVerifyBaseDir(t *testing.T) {
	t.Run("creates and verifies a writable dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "renovate-data")
		t.Setenv("RENOVATE_BASE_DIR", dir)
		if err := newBaseDirVerifier().verify(t.Context()); err != nil {
			t.Fatalf("verify() = %v, want nil", err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("base dir not created: %v", err)
		}
	})
	t.Run("fails when the path is a regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		t.Setenv("RENOVATE_BASE_DIR", file)
		if err := newBaseDirVerifier().verify(t.Context()); err == nil {
			t.Error("verify() = nil, want error when base dir is a file")
		}
	})
}

func TestProbeBaseDirWrite(t *testing.T) {
	t.Run("reports not writable when the probe cannot be created", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory write permissions")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("setup: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := probeBaseDirWrite(t.Context(), dir)
		if err == nil {
			t.Fatal("probeBaseDirWrite() = nil, want error when the write probe cannot be created")
		}
		if !strings.Contains(err.Error(), "not writable") {
			t.Errorf("probeBaseDirWrite() error = %v, want it to mention %q", err, "not writable")
		}
	})
	// A probe that was never ATTEMPTED is not a permission diagnosis: the
	// library returns its error return (as opposed to a stage outcome) only
	// when it created nothing at all, so wording that as "not writable" would
	// blame a perfectly good volume for a cancelled preflight — the exact
	// misreport the hint in logBaseDirError would send an operator chasing.
	t.Run("a cancelled context reports the probe was not attempted, not a verdict", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dir := t.TempDir()

		err := probeBaseDirWrite(ctx, dir)
		if err == nil {
			t.Fatal("probeBaseDirWrite() = nil with a cancelled context, want an error")
		}
		if !strings.Contains(err.Error(), "not attempted") {
			t.Errorf("probeBaseDirWrite() error = %v, want it to mention %q", err, "not attempted")
		}
		if strings.Contains(err.Error(), "not writable") {
			t.Errorf("probeBaseDirWrite() error = %v, must not report a writability verdict", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("probeBaseDirWrite() error = %v, want it to wrap context.Canceled", err)
		}
	})
}

// TestBaseDirProbeStageError pins that every stage of the probe ladder stays
// DISTINGUISHABLE after delegating the ladder to atomicfile: the six stages
// this app's hand-rolled probe reported separately must still produce six
// distinct, stage-naming errors wrapping the filesystem cause, and a passing
// probe must still produce no error. This is the regression guard for the
// adoption itself — a mapping that collapsed two stages onto one message, or
// swallowed a teardown failure the way the hand-rolled predecessor did,
// did, would leave the preflight silently weaker while every other test passed.
func TestBaseDirProbeStageError(t *testing.T) {
	cause := errors.New("boom")
	stages := []struct {
		stage    atomicfile.ProbeStage
		wantText string
	}{
		{atomicfile.ProbeStageMkdir, "mkdir base dir"},
		{atomicfile.ProbeStageCreate, "not writable"},
		{atomicfile.ProbeStageWrite, "write probe file"},
		{atomicfile.ProbeStageSync, "sync probe file"},
		{atomicfile.ProbeStageClose, "close probe file"},
		{atomicfile.ProbeStageRemove, "remove probe file"},
	}

	if err := baseDirProbeStageError("/data", atomicfile.ProbeResult{}); err != nil {
		t.Errorf("baseDirProbeStageError(passing probe) = %v, want nil", err)
	}

	seen := make(map[string]atomicfile.ProbeStage, len(stages))
	for _, s := range stages {
		res := atomicfile.ProbeResult{
			Dir:   "/data",
			Name:  atomicfile.TempName(),
			Stage: s.stage,
			Err:   cause,
		}
		err := baseDirProbeStageError("/data", res)
		if err == nil {
			t.Errorf("baseDirProbeStageError(%s) = nil, want an error", s.stage)
			continue
		}
		if !strings.Contains(err.Error(), s.wantText) {
			t.Errorf("baseDirProbeStageError(%s) = %v, want it to mention %q", s.stage, err, s.wantText)
		}
		if !errors.Is(err, cause) {
			t.Errorf("baseDirProbeStageError(%s) = %v, want it to wrap the filesystem cause", s.stage, err)
		}
		if prev, dup := seen[err.Error()]; dup {
			t.Errorf("stages %s and %s produce the same error %q; each stage must stay distinguishable",
				prev, s.stage, err)
		}
		seen[err.Error()] = s.stage
	}
}

// TestLoadRunTimeout_ZeroIsNonPositiveAndUsesDefault pins the non-positive
// boundary of loadRunTimeout. A zero RUN_TIMEOUT parses cleanly (err ==
// nil) but is non-positive, so it must fall back to the default rather than
// arm a zero timeout that would cancel every run immediately; the boundary
// must treat zero like a negative, never let it through unchanged.
func TestLoadRunTimeout_ZeroIsNonPositiveAndUsesDefault(t *testing.T) {
	t.Run("bare zero", func(t *testing.T) {
		t.Setenv("RUN_TIMEOUT", "0")

		got := loadRunTimeout()

		if got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() with RUN_TIMEOUT=0 = %v, want %v (zero is non-positive and must use the default)", got, defaultRunTimeout)
		}
	})
	t.Run("zero seconds", func(t *testing.T) {
		t.Setenv("RUN_TIMEOUT", "0s")

		got := loadRunTimeout()

		if got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() with RUN_TIMEOUT=0s = %v, want %v (zero is non-positive and must use the default)", got, defaultRunTimeout)
		}
	})
}

func TestSetupLogger_MapsLogLevelEnvToHandlerLevel(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		enabled  slog.Level
		disabled slog.Level
	}{
		{"debug enables debug", "debug", slog.LevelDebug, slog.LevelDebug - 1},
		{"info enables info disables debug", "info", slog.LevelInfo, slog.LevelDebug},
		{"warn enables warn disables info", "warn", slog.LevelWarn, slog.LevelInfo},
		{"error enables error disables warn", "error", slog.LevelError, slog.LevelWarn},
		{"unknown falls back to info", "bogus", slog.LevelInfo, slog.LevelDebug},
		{"empty falls back to info", "", slog.LevelInfo, slog.LevelDebug},
		{"surrounding whitespace trimmed", "  warn  ", slog.LevelWarn, slog.LevelInfo},
	}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tt.env)
			setupLogger()
			ctx := t.Context()
			if !slog.Default().Enabled(ctx, tt.enabled) {
				t.Errorf("setupLogger() with LOG_LEVEL=%q: level %v not enabled", tt.env, tt.enabled)
			}
			if slog.Default().Enabled(ctx, tt.disabled) {
				t.Errorf("setupLogger() with LOG_LEVEL=%q: level %v enabled, want disabled", tt.env, tt.disabled)
			}
		})
	}
}

// TestVerifyBaseDirAt_TimesOutWhileProbeSlotHeld pins the hung-filesystem
// containment: when a previous probe goroutine is still wedged (the slot is
// held) and the caller's budget expires, verifyAt reports a timeout
// instead of blocking — and once the wedged probe releases the slot, later
// verifications succeed again.
func TestVerifyBaseDirAt_TimesOutWhileProbeSlotHeld(t *testing.T) {
	t.Parallel()
	verifier := newBaseDirVerifier()
	verifier.slot <- struct{}{} // a prior probe is wedged on a hung filesystem
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller's verification budget is already exhausted

	err := verifier.verifyAt(ctx, t.TempDir())

	<-verifier.slot // the wedged probe finally finishes
	if err == nil {
		t.Fatal("verifyAt() = nil with the probe slot held and the context done, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("verifyAt() error = %v, want it to mention %q", err, "timed out")
	}

	if err := verifier.verifyAt(t.Context(), t.TempDir()); err != nil {
		t.Errorf("verifyAt() = %v after the slot was released, want nil (the slot must be reusable)", err)
	}
}

// TestVerifyBaseDirAt_DerivedDeadlineBoundsSlotWait pins the derived deadline
// itself: with the slot held and a LIVE parent context, the wait must expire on
// the probe budget rather than block forever. Under synctest the budget elapses
// on virtual time, so removing the deadline deadlocks the bubble and fails here
// instead of costing ten wall-clock seconds.
func TestVerifyBaseDirAt_DerivedDeadlineBoundsSlotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		verifier := newBaseDirVerifier()
		verifier.slot <- struct{}{}

		err := verifier.verifyAt(t.Context(), t.TempDir())

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("verifyAt() with a live parent and held slot = %v, want context.DeadlineExceeded", err)
		}
	})
}

// captureSetupLoggerOutput installs the real handler over a pipe and returns
// what setupLogger wrote there for the given LOG_LEVEL. Not convertible to
// capture.Default: this drives setupLogger itself, so the handler under test
// is the one it installs.
func captureSetupLoggerOutput(t *testing.T, level string) string {
	t.Helper()
	prevLogger := slog.Default()
	prevStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	t.Cleanup(func() {
		os.Stderr = prevStderr
		slog.SetDefault(prevLogger)
		_ = r.Close()
		_ = w.Close()
	})

	t.Setenv("LOG_LEVEL", level)
	os.Stderr = w
	setupLogger()
	os.Stderr = prevStderr
	slog.SetDefault(prevLogger)
	if err := w.Close(); err != nil {
		t.Fatalf("close captured stderr = %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr = %v", err)
	}
	return string(out)
}

func TestSetupLogger_WarnsOnlyForNonEmptyUnrecognizedLevel(t *testing.T) {
	t.Run("unrecognized value names the fallback", func(t *testing.T) {
		logs := captureSetupLoggerOutput(t, "  bogus  ")
		if !strings.Contains(logs, `level=WARN msg="unrecognized LOG_LEVEL, using default"`) {
			t.Errorf("setupLogger() log = %q, want the malformed-level warning", logs)
		}
		if !strings.Contains(logs, `value="  bogus  "`) || !strings.Contains(logs, "default=info") {
			t.Errorf("setupLogger() log = %q, want the raw value echoed and default=info", logs)
		}
	})

	t.Run("empty value is a silent recognized default", func(t *testing.T) {
		logs := captureSetupLoggerOutput(t, "")
		if strings.Contains(logs, "unrecognized LOG_LEVEL") {
			t.Errorf("setupLogger() with empty LOG_LEVEL logged %q, want no malformed-level warning", logs)
		}
	})
}
