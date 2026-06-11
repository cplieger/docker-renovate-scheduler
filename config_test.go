package main

import (
	"context"
	"os"
	"path/filepath"
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
