package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
)

// defaultImageUID is the non-root user baked into the renovate base image
// (passwd entry `ubuntu:x:12021:0`). Running as this UID, or as root, gives
// containerbase a writable home and tool directory, so Renovate installs
// toolchains and regenerates lockfiles with no extra configuration.
const defaultImageUID = 12021

// warnIfRootlessCacheUnwritable emits a loud startup warning when the container
// is run as a non-default UID without redirecting its tool caches. A custom UID
// has no writable containerbase home, so Renovate's language-tool caches
// (go.sum / package-lock.json regeneration) fail silently: the dependency PR is
// raised manifest-only and breaks the consuming repo's CI days later. Surfacing
// it at startup turns that delayed, cross-repo failure into an immediate, local
// signal. Advisory only; it never blocks startup.
//
// Two flavors: no RENOVATE_CUSTOM_ENV_VARIABLES at all gets the loud warning;
// a RENOVATE_CUSTOM_ENV_VARIABLES that names no cache/path variable (a
// proxy-only forwarding setup, Renovate's other documented use for it) gets a
// softer one — the caches are still unredirected, the operator just already
// knows the mitigation mechanism.
func warnIfRootlessCacheUnwritable() {
	switch rootlessCacheRisk(os.Geteuid(), os.Getenv) {
	case rootlessRiskNone:
	case rootlessRiskNoRedirection:
		slog.Warn("running as a non-default UID with no tool-cache redirection; a "+
			"custom UID has no writable containerbase home, so artifact/lockfile "+
			"regeneration (go.sum, package-lock.json) will likely fail and dependency "+
			"PRs will be raised with stale lockfiles that break the consuming repo's CI",
			"uid", os.Geteuid(),
			"home", os.Getenv("HOME"),
			"fix", "run as the image's default UID 12021, or set RENOVATE_BINARY_SOURCE=global "+
				"and redirect each tool cache to a writable volume, forwarding it to Renovate's "+
				"artifact subprocesses via RENOVATE_CUSTOM_ENV_VARIABLES (or a config.js "+
				"customEnvVariables) — see the README, 'Running as a non-default user'")
	case rootlessRiskNoCacheVars:
		// Key names only: the forwarded values can carry credentials (an
		// HTTP_PROXY with basic auth), so they must never reach the log.
		slog.Warn("running as a non-default UID and RENOVATE_CUSTOM_ENV_VARIABLES "+
			"redirects no tool cache; a custom UID has no writable containerbase home, "+
			"so artifact/lockfile regeneration (go.sum, package-lock.json) will likely "+
			"fail and dependency PRs will be raised with stale lockfiles that break the "+
			"consuming repo's CI",
			"uid", os.Geteuid(),
			"home", os.Getenv("HOME"),
			"custom_env_vars", strings.Join(customEnvVarNames(os.Getenv("RENOVATE_CUSTOM_ENV_VARIABLES")), ","),
			"fix", "add each redirected tool-cache variable (GOCACHE, npm_config_cache, …) "+
				"to RENOVATE_CUSTOM_ENV_VARIABLES so it reaches Renovate's artifact "+
				"subprocesses — see the README, 'Running as a non-default user'")
	}
}

// rootlessRisk classifies the startup cache-writability posture.
type rootlessRisk int

const (
	// rootlessRiskNone: default/root UID, or a custom UID whose
	// RENOVATE_CUSTOM_ENV_VARIABLES names at least one cache/path variable
	// (the documented mitigation).
	rootlessRiskNone rootlessRisk = iota
	// rootlessRiskNoRedirection: custom UID and no
	// RENOVATE_CUSTOM_ENV_VARIABLES at all.
	rootlessRiskNoRedirection
	// rootlessRiskNoCacheVars: custom UID and a RENOVATE_CUSTOM_ENV_VARIABLES
	// that names no cache/path variable (e.g. proxy forwarding only), so the
	// tool caches are still unredirected.
	rootlessRiskNoCacheVars
)

// rootlessCacheRisk is the pure decision behind the warning, split out for
// testing. getenv is injected so tests can exercise the matrix without
// changing the real UID or environment.
//
// It deliberately does NOT probe the filesystem. The base image's entrypoint
// sets HOME=/home/ubuntu for every UID, but that directory is writable only by
// the image's own user (12021), so a write-probe of HOME would be both
// redundant with the UID check and a needless side effect. "Non-default UID
// without cache redirection" is the reliable, side-effect-free signal.
//
// RENOVATE_CUSTOM_ENV_VARIABLES is the documented mitigation: it is what forwards the
// redirected caches to Renovate's artifact subprocesses (go/npm), where lockfile
// regeneration runs. GOCACHE / npm_config_cache set on the scheduler alone do NOT reach
// those subprocesses (Renovate forwards only an allowlist -- GOPATH yes, those two no),
// so they are not a reliable "caches work" signal and must not suppress on their own.
// Presence alone does not suppress either: Renovate's docs also recommend
// customEnvVariables for proxy forwarding (HTTP_PROXY/NO_PROXY), and a proxy-only
// value redirects no cache — the JSON must name at least one cache/path variable.
// (Redirecting via a Renovate config.js customEnvVariables is equally valid but not
// visible here, so that setup sees a benign false-positive.)
func rootlessCacheRisk(euid int, getenv func(string) string) rootlessRisk {
	if euid == defaultImageUID || euid == 0 {
		return rootlessRiskNone
	}
	raw := getenv("RENOVATE_CUSTOM_ENV_VARIABLES")
	if raw == "" {
		return rootlessRiskNoRedirection
	}
	if slices.ContainsFunc(customEnvVarNames(raw), cacheLikeEnvVar) {
		return rootlessRiskNone
	}
	return rootlessRiskNoCacheVars
}

// customEnvVarNames extracts the variable names from the
// RENOVATE_CUSTOM_ENV_VARIABLES JSON object. Undecodable input yields no
// names: a value Renovate itself cannot parse redirects no cache (Renovate
// fails loudly on it at startup on its own).
func customEnvVarNames(raw string) []string {
	var vars map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &vars); err != nil {
		return nil
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	return names
}

// cacheLikeEnvVar reports whether an environment variable name plausibly
// redirects a tool cache or toolchain home. The set is unbounded across
// language managers, so this is a heuristic: any name containing "cache"
// (GOCACHE, npm_config_cache, PIP_CACHE_DIR, YARN_CACHE_FOLDER, …) plus the
// well-known path/home variables that don't. A false negative here only
// softens nothing — the warning stays advisory — and a false positive only
// suppresses an advisory line.
func cacheLikeEnvVar(name string) bool {
	if strings.Contains(strings.ToUpper(name), "CACHE") {
		return true
	}
	switch name {
	case "HOME", "GOPATH", "CARGO_HOME", "GRADLE_USER_HOME", "COMPOSER_HOME":
		return true
	}
	return false
}
