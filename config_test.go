package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			t.Setenv("SCHED_INTERVAL", tt.env)
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
		t.Setenv("SCHED_TIMEOUT", "")
		if got := loadRunTimeout(); got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() = %v, want %v", got, defaultRunTimeout)
		}
	})
	t.Run("parsed value", func(t *testing.T) {
		t.Setenv("SCHED_TIMEOUT", "30m")
		if got := loadRunTimeout(); got != 30*time.Minute {
			t.Errorf("loadRunTimeout() = %v, want 30m", got)
		}
	})
	t.Run("default on garbage", func(t *testing.T) {
		t.Setenv("SCHED_TIMEOUT", "not-a-duration")
		if got := loadRunTimeout(); got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() = %v, want %v", got, defaultRunTimeout)
		}
	})
	t.Run("default on non-positive", func(t *testing.T) {
		t.Setenv("SCHED_TIMEOUT", "-5m")
		if got := loadRunTimeout(); got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() = %v, want %v", got, defaultRunTimeout)
		}
	})
}

func TestGetEnv(t *testing.T) {
	t.Setenv("TEST_RENOVATE_ENV", "value")
	if got := getEnv("TEST_RENOVATE_ENV", "fallback"); got != "value" {
		t.Errorf("getEnv = %q, want value", got)
	}
	t.Setenv("TEST_RENOVATE_ENV", "")
	if got := getEnv("TEST_RENOVATE_ENV", "fallback"); got != "fallback" {
		t.Errorf("getEnv = %q, want fallback", got)
	}
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

func TestVerifyBaseDir(t *testing.T) {
	t.Run("creates and verifies a writable dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "renovate-data")
		t.Setenv("RENOVATE_BASE_DIR", dir)
		if err := verifyBaseDir(context.Background()); err != nil {
			t.Fatalf("verifyBaseDir() = %v, want nil", err)
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
		if err := verifyBaseDir(context.Background()); err == nil {
			t.Error("verifyBaseDir() = nil, want error when base dir is a file")
		}
	})
}

// TestVerifyBaseDir_ReportsNotWritableWhenDirExistsButUnwritable covers the
// os.Create error branch (config.go ~line 173): the base dir already exists
// (so MkdirAll is a no-op) but is not writable, so creating .write_test fails
// and verifyBaseDir must report "not writable" -- distinct from the "mkdir"
// error the existing regular-file case exercises. Skipped as root, which
// bypasses directory write permissions and makes the branch unreachable.
func TestVerifyBaseDir_ReportsNotWritableWhenDirExistsButUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions; the unwritable-dir branch is unreachable as root")
	}
	dir := filepath.Join(t.TempDir(), "ro-base")
	if err := os.Mkdir(dir, 0o555); err != nil { // readable + executable, not writable
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // let TempDir cleanup remove it
	t.Setenv("RENOVATE_BASE_DIR", dir)

	err := verifyBaseDir(context.Background())
	if err == nil {
		t.Fatal("verifyBaseDir() = nil, want error when the base dir exists but is not writable")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("verifyBaseDir() error = %v, want it to mention \"not writable\"", err)
	}
}

// TestLoadRunTimeout_ZeroIsNonPositiveAndUsesDefault pins the `d <= 0`
// boundary on config.go:140. A zero SCHED_TIMEOUT parses cleanly (err == nil)
// but is non-positive, so it must fall back to the default rather than arm a
// zero timeout that would cancel every run immediately. A boundary mutation
// (`d < 0`) would let a zero duration through unchanged.
func TestLoadRunTimeout_ZeroIsNonPositiveAndUsesDefault(t *testing.T) {
	t.Run("bare zero", func(t *testing.T) {
		t.Setenv("SCHED_TIMEOUT", "0")

		got := loadRunTimeout()

		if got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() with SCHED_TIMEOUT=0 = %v, want %v (zero is non-positive and must use the default)", got, defaultRunTimeout)
		}
	})
	t.Run("zero seconds", func(t *testing.T) {
		t.Setenv("SCHED_TIMEOUT", "0s")

		got := loadRunTimeout()

		if got != defaultRunTimeout {
			t.Errorf("loadRunTimeout() with SCHED_TIMEOUT=0s = %v, want %v (zero is non-positive and must use the default)", got, defaultRunTimeout)
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
		{"warning alias enables warn", "warning", slog.LevelWarn, slog.LevelInfo},
		{"error enables error disables warn", "error", slog.LevelError, slog.LevelWarn},
		{"unknown falls back to info", "bogus", slog.LevelInfo, slog.LevelDebug},
		{"empty falls back to info", "", slog.LevelInfo, slog.LevelDebug},
		{"uppercase is case-insensitive", "ERROR", slog.LevelError, slog.LevelWarn},
		{"surrounding whitespace trimmed", "  warn  ", slog.LevelWarn, slog.LevelInfo},
	}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tt.env)
			setupLogger()
			ctx := context.Background()
			if !slog.Default().Enabled(ctx, tt.enabled) {
				t.Errorf("setupLogger() with LOG_LEVEL=%q: level %v not enabled", tt.env, tt.enabled)
			}
			if slog.Default().Enabled(ctx, tt.disabled) {
				t.Errorf("setupLogger() with LOG_LEVEL=%q: level %v enabled, want disabled", tt.env, tt.disabled)
			}
		})
	}
}
