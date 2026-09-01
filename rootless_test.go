package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// Cache forwarding is name-only; forwarded values are opaque.
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
		// UID gates: the writable-home UIDs are silent whatever the env says.
		{
			name: "default image UID never warns",
			euid: defaultImageUID, getenv: noEnv, want: rootlessRiskNone,
		},
		{
			name: "root never warns",
			euid: 0, getenv: noEnv, want: rootlessRiskNone,
		},
		// Mechanism not engaged: no RENOVATE_CUSTOM_ENV_VARIABLES at all.
		// Plain scheduler-side cache env vars do not count: Renovate forwards
		// only an allowlist to artifact subprocesses, so they prove nothing.
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
		// Mechanism engaged, no cache/path variable named: soft warning.
		// Undecodable and empty-object input land here too — they name
		// nothing, and Renovate itself fails loudly on JSON it cannot parse.
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
		// Mechanism engaged: a cache/path variable is NAMED, so the warning is
		// suppressed regardless of value — the value-blind rows pin the line
		// in the sand, not an oversight.
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
			name: "value-blind by contract: empty-string cache value still suppresses",
			euid: customUID, getenv: custom(`{"GOCACHE":""}`), want: rootlessRiskNone,
		},
		{
			name: "value-blind by contract: null cache value still suppresses",
			euid: customUID, getenv: custom(`{"GOCACHE":null}`), want: rootlessRiskNone,
		},
		{
			name: "value-blind by contract: non-string cache value still suppresses",
			euid: customUID, getenv: custom(`{"GOCACHE":123}`), want: rootlessRiskNone,
		},
		{
			name: "value-blind by contract: HOME=/ still suppresses",
			euid: customUID, getenv: custom(`{"HOME":"/"}`), want: rootlessRiskNone,
		},
		{
			name: "open-ended heuristic: any CACHE-containing name suppresses (CACHE_BUST)",
			euid: customUID, getenv: custom(`{"CACHE_BUST":"1"}`), want: rootlessRiskNone,
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

// TestCacheLikeEnvVar pins the name heuristic exactly as documented: an
// open-ended CACHE substring (which deliberately also matches cache-control
// style names — the accepted false-positive direction) plus the well-known
// path/home variables.
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
		{"CACHE_BUST", true},
		{"NO_CACHE", true},
		{"HTTP_PROXY", false},
		{"NO_PROXY", false},
		{"RENOVATE_TOKEN", false},
		{"RUSTUP_HOME", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := cacheLikeEnvVar(tc.name); got != tc.want {
			t.Errorf("cacheLikeEnvVar(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func requireCustomUID(t *testing.T) int {
	t.Helper()
	euid := os.Geteuid()
	if euid == 0 || euid == defaultImageUID {
		t.Skip("running as root or the image default UID; the warning branch is unreachable for this process")
	}
	return euid
}

// requireSingleWarning asserts exactly one Warn record contains message and
// returns it.
func requireSingleWarning(t *testing.T, rec *capture.Recorder, message string) slog.Record {
	t.Helper()
	var match slog.Record
	matches := 0
	for _, record := range rec.Records() {
		if record.Level == slog.LevelWarn && strings.Contains(record.Message, message) {
			match = record
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("warnings containing %q = %d, want 1; records = %v", message, matches, rec.Messages())
	}
	return match
}

// requireRecordAttr asserts the record carries the attribute and returns its
// value.
func requireRecordAttr(t *testing.T, record slog.Record, key string) slog.Value {
	t.Helper()
	var value slog.Value
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			value = attr.Value
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("warning has no %q attribute", key)
	}
	return value
}

// leakedRecord returns the first captured message or attribute value
// containing forbidden, with the attribute key or "message" as key.
func leakedRecord(rec *capture.Recorder, forbidden string) (key, value string, found bool) {
	for _, record := range rec.Records() {
		if strings.Contains(record.Message, forbidden) {
			return "message", record.Message, true
		}
		record.Attrs(func(attr slog.Attr) bool {
			if !found && strings.Contains(attr.Value.String(), forbidden) {
				key, value, found = attr.Key, attr.Value.String(), true
			}
			return !found
		})
		if found {
			return key, value, true
		}
	}
	return "", "", false
}

func TestWarnIfRootlessCacheUnwritable_NoRedirectionEmitsActionableWarning(t *testing.T) {
	euid := requireCustomUID(t)
	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", "")
	rec := capture.Default(t)

	warnIfRootlessCacheUnwritable()

	warning := requireSingleWarning(t, rec, "no tool-cache redirection")
	if got := requireRecordAttr(t, warning, "uid").Int64(); got != int64(euid) {
		t.Errorf("warning uid = %d, want process uid %d", got, euid)
	}
	fix := requireRecordAttr(t, warning, "fix").String()
	if !strings.Contains(fix, "RENOVATE_CUSTOM_ENV_VARIABLES") {
		t.Errorf("warning fix hint = %q, want it to name RENOVATE_CUSTOM_ENV_VARIABLES", fix)
	}
}

func TestWarnIfRootlessCacheUnwritable_ProxyOnlyWarnsWithoutLeakingValue(t *testing.T) {
	requireCustomUID(t)
	const proxyValue = "http://user:secret@proxy:3128"
	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", `{"HTTP_PROXY":"`+proxyValue+`"}`)
	rec := capture.Default(t)

	warnIfRootlessCacheUnwritable()

	warning := requireSingleWarning(t, rec, "redirects no tool cache")
	keys := requireRecordAttr(t, warning, "custom_env_vars").String()
	if !strings.Contains(keys, "HTTP_PROXY") {
		t.Errorf("soft warning custom_env_vars = %q, want it to name HTTP_PROXY", keys)
	}
	if key, value, found := leakedRecord(rec, "secret"); found {
		t.Errorf("warnIfRootlessCacheUnwritable() leaked the value in %s = %q", key, value)
	}
}

func TestWarnIfRootlessCacheUnwritable_CacheRedirectionSuppressesWarning(t *testing.T) {
	requireCustomUID(t)
	t.Setenv("RENOVATE_CUSTOM_ENV_VARIABLES", `{"GOCACHE":"/data/.cache/go-build"}`)
	rec := capture.Default(t)

	warnIfRootlessCacheUnwritable()

	if got := rec.Len(); got != 0 {
		t.Errorf("cache-naming environment emitted %d records, want 0", got)
	}
}
