package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cplieger/slogx/capture"
)

func TestRootlessCacheLikelyUnwritable(t *testing.T) {
	const customUID = 568

	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	noEnv := env(nil)

	tests := []struct {
		getenv func(string) string
		name   string
		euid   int
		want   bool
	}{
		{
			name: "default image UID never warns",
			euid: defaultImageUID, getenv: noEnv, want: false,
		},
		{
			name: "root never warns",
			euid: 0, getenv: noEnv, want: false,
		},
		{
			name: "custom UID with no cache redirection warns",
			euid: customUID, getenv: noEnv, want: true,
		},
		{
			name: "custom UID with GOCACHE but no forwarding still warns",
			euid: customUID, getenv: env(map[string]string{"GOCACHE": "/data/.cache/go-build"}), want: true,
		},
		{
			name: "custom UID with npm_config_cache but no forwarding still warns",
			euid: customUID, getenv: env(map[string]string{"npm_config_cache": "/data/.npm"}), want: true,
		},
		{
			name: "custom UID mitigated via RENOVATE_CUSTOM_ENV_VARIABLES does not warn",
			euid: customUID, getenv: env(map[string]string{"RENOVATE_CUSTOM_ENV_VARIABLES": `{"GOCACHE":"/data/.cache/go-build"}`}), want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rootlessCacheLikelyUnwritable(tc.euid, tc.getenv)
			if got != tc.want {
				t.Errorf("rootlessCacheLikelyUnwritable(euid=%d) = %v, want %v", tc.euid, got, tc.want)
			}
		})
	}
}

// TestWarnIfRootlessCacheUnwritable_EmitsAndSuppressesWarning pins the
// wrapper's observable contract on captured slog output: an unmitigated
// non-default UID emits exactly one startup warning carrying the uid and the
// remediation hint, and the documented mitigation suppresses it entirely.
// The pure decision matrix is TestRootlessCacheLikelyUnwritable's; this test
// pins the wiring (real euid + real env reach the decision, and the Warn
// actually fires). Skipped when the test process runs as root or the image
// UID, where the warning branch is unreachable by design.
func TestWarnIfRootlessCacheUnwritable_EmitsAndSuppressesWarning(t *testing.T) {
	euid := os.Geteuid()
	if euid == 0 || euid == defaultImageUID {
		t.Skip("running as root or the image default UID; the warning branch is unreachable for this process")
	}

	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", "")
	rec := capture.Default(t)
	warnIfRootlessCacheUnwritable()
	var warn *slog.Record
	for _, r := range rec.Records() {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "no tool-cache redirection") {
			warn = &r
			break
		}
	}
	if warn == nil {
		t.Fatalf("unmitigated custom UID emitted no matching warning; records = %v", rec.Messages())
	}
	fixHint := ""
	warn.Attrs(func(a slog.Attr) bool {
		if a.Key == "fix" {
			fixHint = a.Value.String()
			return false
		}
		return true
	})
	if !strings.Contains(fixHint, "RENOVATE_CUSTOM_ENV_VARIABLES") {
		t.Errorf("warning fix hint = %q, want it to name RENOVATE_CUSTOM_ENV_VARIABLES; the remediation must be actionable from the log line", fixHint)
	}

	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", `{"GOCACHE":"/data/.cache/go-build"}`)
	rec2 := capture.Default(t)
	warnIfRootlessCacheUnwritable()
	if n := rec2.Len(); n != 0 {
		t.Errorf("mitigated environment emitted %d records, want 0 (RENOVATE_CUSTOM_ENV_VARIABLES suppresses the warning)", n)
	}
}
