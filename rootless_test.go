package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// TestRootlessCacheRisk pins the classification strategy tier by tier (see
// rootless.go's section comment): UID gates, mechanism-not-engaged (loud),
// engaged-without-a-cache-name (soft), and engaged (silent). The "engaged"
// rows deliberately sweep every value shape — real path, empty string, null,
// number, HOME:"/", a cache-control-style name — because value-blindness IS
// the contract (h-f1, 2026-07-22): engagement is judged by NAMES only, and
// value correctness stays the operator's responsibility. If one of those
// rows ever fails, the line in the sand moved; that must be a deliberate,
// user-approved decision, not a drive-by.
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

// The TestWarnIfRootlessCacheUnwritable_* tests pin the wrapper's observable
// contract on captured slog output. The pure decision matrix is
// TestRootlessCacheRisk's; these pin the wiring (real euid + real env reach
// the decision, and the right Warn actually fires). Skipped when the test
// process runs as root or the image UID, where the warning branches are
// unreachable by design. They capture the process-global slog default, so
// they must stay serial (no t.Parallel).

// requireCustomUID skips the test when the warning branch is unreachable for
// this process and returns the effective UID otherwise.
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

// requireRecordsExclude asserts no captured message or attribute value
// contains the forbidden substring.
func requireRecordsExclude(t *testing.T, rec *capture.Recorder, forbidden string) {
	t.Helper()
	for _, record := range rec.Records() {
		if strings.Contains(record.Message, forbidden) {
			t.Errorf("warning message leaked %q: %q", forbidden, record.Message)
		}
		record.Attrs(func(attr slog.Attr) bool {
			if strings.Contains(attr.Value.String(), forbidden) {
				t.Errorf("warning attr %s leaked %q: %q", attr.Key, forbidden, attr.Value.String())
			}
			return true
		})
	}
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
	requireRecordsExclude(t, rec, "secret")
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
