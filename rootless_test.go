package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cplieger/slogx/capture"
)

func TestRootlessCacheRisk(t *testing.T) {
	const customUID = 568

	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	noEnv := env(nil)
	custom := func(v string) func(string) string {
		return env(map[string]string{"RENOVATE_CUSTOM_ENV_VARIABLES": v})
	}

	tests := []struct {
		getenv func(string) string
		name   string
		euid   int
		want   rootlessRisk
	}{
		{
			name: "default image UID never warns",
			euid: defaultImageUID, getenv: noEnv, want: rootlessRiskNone,
		},
		{
			name: "root never warns",
			euid: 0, getenv: noEnv, want: rootlessRiskNone,
		},
		{
			name: "custom UID with no cache redirection warns loudly",
			euid: customUID, getenv: noEnv, want: rootlessRiskNoRedirection,
		},
		{
			name: "custom UID with GOCACHE but no forwarding still warns loudly",
			euid: customUID, getenv: env(map[string]string{"GOCACHE": "/data/.cache/go-build"}), want: rootlessRiskNoRedirection,
		},
		{
			name: "custom UID with npm_config_cache but no forwarding still warns loudly",
			euid: customUID, getenv: env(map[string]string{"npm_config_cache": "/data/.npm"}), want: rootlessRiskNoRedirection,
		},
		{
			name: "custom UID forwarding a cache variable does not warn",
			euid: customUID, getenv: custom(`{"GOCACHE":"/data/.cache/go-build"}`), want: rootlessRiskNone,
		},
		{
			name: "custom UID forwarding GOPATH (path variable, no CACHE substring) does not warn",
			euid: customUID, getenv: custom(`{"GOPATH":"/data/go"}`), want: rootlessRiskNone,
		},
		{
			name: "custom UID forwarding a lowercase cache name does not warn",
			euid: customUID, getenv: custom(`{"npm_config_cache":"/data/.npm"}`), want: rootlessRiskNone,
		},
		{
			name: "custom UID forwarding proxies only warns softly",
			euid: customUID, getenv: custom(`{"HTTP_PROXY":"http://proxy:3128","NO_PROXY":"localhost"}`), want: rootlessRiskNoCacheVars,
		},
		{
			name: "custom UID with undecodable JSON warns softly (redirects no cache)",
			euid: customUID, getenv: custom(`not json`), want: rootlessRiskNoCacheVars,
		},
		{
			name: "custom UID with empty JSON object warns softly",
			euid: customUID, getenv: custom(`{}`), want: rootlessRiskNoCacheVars,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rootlessCacheRisk(tc.euid, tc.getenv)
			if got != tc.want {
				t.Errorf("rootlessCacheRisk(euid=%d) = %v, want %v", tc.euid, got, tc.want)
			}
		})
	}
}

func TestCacheLikeEnvVar(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"GOCACHE", true},
		{"npm_config_cache", true},
		{"PIP_CACHE_DIR", true},
		{"YARN_CACHE_FOLDER", true},
		{"GOPATH", true},
		{"HOME", true},
		{"CARGO_HOME", true},
		{"GRADLE_USER_HOME", true},
		{"COMPOSER_HOME", true},
		{"HTTP_PROXY", false},
		{"NO_PROXY", false},
		{"RENOVATE_TOKEN", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := cacheLikeEnvVar(tc.name); got != tc.want {
			t.Errorf("cacheLikeEnvVar(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWarnIfRootlessCacheUnwritable_EmitsAndSuppressesWarning pins the
// wrapper's observable contract on captured slog output: an unmitigated
// non-default UID emits exactly one loud startup warning carrying the uid and
// the remediation hint, a proxy-only RENOVATE_CUSTOM_ENV_VARIABLES emits the
// softer no-cache-vars warning (naming the forwarded keys but never their
// values — they can carry proxy credentials), and a cache-naming value
// suppresses everything. The pure decision matrix is TestRootlessCacheRisk's;
// this test pins the wiring (real euid + real env reach the decision, and the
// right Warn actually fires). Skipped when the test process runs as root or
// the image UID, where the warning branches are unreachable by design.
func TestWarnIfRootlessCacheUnwritable_EmitsAndSuppressesWarning(t *testing.T) {
	euid := os.Geteuid()
	if euid == 0 || euid == defaultImageUID {
		t.Skip("running as root or the image default UID; the warning branch is unreachable for this process")
	}

	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", "")
	rec := capture.Default(t)
	warnIfRootlessCacheUnwritable()
	warns := 0
	warningUID := int64(-1)
	fixHint := ""
	for _, r := range rec.Records() {
		if r.Level != slog.LevelWarn || !strings.Contains(r.Message, "no tool-cache redirection") {
			continue
		}
		warns++
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "uid":
				warningUID = a.Value.Int64()
			case "fix":
				fixHint = a.Value.String()
			}
			return true
		})
	}
	if warns != 1 {
		t.Fatalf("unmitigated custom UID emitted %d matching warnings, want exactly 1; records = %v", warns, rec.Messages())
	}
	if warningUID != int64(euid) {
		t.Errorf("warning uid = %d, want process uid %d", warningUID, euid)
	}
	if !strings.Contains(fixHint, "RENOVATE_CUSTOM_ENV_VARIABLES") {
		t.Errorf("warning fix hint = %q, want it to name RENOVATE_CUSTOM_ENV_VARIABLES; the remediation must be actionable from the log line", fixHint)
	}

	const proxyValue = "http://user:secret@proxy:3128"
	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", `{"HTTP_PROXY":"`+proxyValue+`"}`)
	recSoft := capture.Default(t)
	warnIfRootlessCacheUnwritable()
	softWarns := 0
	namedKeys := ""
	for _, r := range recSoft.Records() {
		if r.Level != slog.LevelWarn || !strings.Contains(r.Message, "redirects no tool cache") {
			continue
		}
		softWarns++
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "custom_env_vars" {
				namedKeys = a.Value.String()
			}
			return true
		})
	}
	if softWarns != 1 {
		t.Fatalf("proxy-only forwarding emitted %d soft warnings, want exactly 1; records = %v", softWarns, recSoft.Messages())
	}
	if !strings.Contains(namedKeys, "HTTP_PROXY") {
		t.Errorf("soft warning custom_env_vars = %q, want it to name HTTP_PROXY", namedKeys)
	}
	for _, m := range recSoft.Messages() {
		if strings.Contains(m, "secret") {
			t.Fatalf("soft warning leaked a forwarded value: %q", m)
		}
	}
	for _, r := range recSoft.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), "secret") {
				t.Errorf("soft warning attr %s leaked a forwarded value: %q", a.Key, a.Value.String())
			}
			return true
		})
	}

	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", `{"GOCACHE":"/data/.cache/go-build"}`)
	rec2 := capture.Default(t)
	warnIfRootlessCacheUnwritable()
	if n := rec2.Len(); n != 0 {
		t.Errorf("cache-naming environment emitted %d records, want 0 (RENOVATE_CUSTOM_ENV_VARIABLES naming a cache suppresses the warning)", n)
	}
}
