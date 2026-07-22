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

// --- Rootless cache-risk classification ---
//
// Purpose: a custom (non-root, non-12021) UID has no writable containerbase
// home, so Renovate's artifact/lockfile regeneration fails SILENTLY — the
// dependency PR is raised manifest-only and breaks the consuming repo's CI
// days later. One advisory startup warning turns that delayed, cross-repo
// failure into an immediate, local signal. Advisory only; never blocks boot.
//
// Strategy (one parse, one decision, values never read):
//
//	parse    RENOVATE_CUSTOM_ENV_VARIABLES -> sorted JSON-object key names
//	         (undecodable or non-object input yields no names)
//	classify (euid, names) ->
//	         default/root UID          -> silent
//	         variable unset            -> loud warning  (mechanism not engaged)
//	         >=1 cache-like name       -> silent        (mechanism engaged)
//	         otherwise                 -> soft warning  (engaged, no cache named)
//
// The line in the sand (decided 2026-07-22, deferred finding h-f1 dismissed):
// the classifier verifies MECHANISM ENGAGEMENT only — RENOVATE_CUSTOM_ENV_VARIABLES
// is the documented channel that forwards redirections to Renovate's artifact
// subprocesses, and naming a cache/path variable there is the operator
// engaging it. It never judges value correctness: no string test separates a
// working redirection from a typo'd or unmounted path (only a filesystem
// probe could, and the classifier is deliberately side-effect-free), so
// rejecting "" while blessing "/typo" would just blur whose job config
// correctness is — it is the operator's. The bar for any stricter guard
// here: it must cover its whole input class, or have security impact, or
// prevent a crash. An advisory log line meets none of those.
//
// Accepted error directions, all costing one advisory line: a cache-like
// name that redirects nothing suppresses the warning; plain GOCACHE /
// npm_config_cache env vars on the scheduler do NOT suppress (Renovate
// forwards only an allowlist to artifact subprocesses — GOPATH yes, those
// two no — so they are not a "caches work" signal); and a redirection done
// via a Renovate config.js customEnvVariables is invisible here, drawing a
// spurious loud warning.

// rootlessRisk is the classification result.
type rootlessRisk int

const (
	// rootlessRiskNone: default/root UID, or the mitigation mechanism is
	// engaged (a cache/path variable is named).
	rootlessRiskNone rootlessRisk = iota
	// rootlessRiskNoRedirection: custom UID, RENOVATE_CUSTOM_ENV_VARIABLES unset.
	rootlessRiskNoRedirection
	// rootlessRiskNoCacheVars: custom UID, variable set but naming no
	// cache/path variable (proxy-only forwarding, `{}`, or undecodable input).
	rootlessRiskNoCacheVars
)

// warnIfRootlessCacheUnwritable classifies at boot and emits the tier's
// warning. Log lines carry key NAMES only — forwarded values can hold
// credentials (an HTTP_PROXY with basic auth) and must never reach the log.
func warnIfRootlessCacheUnwritable() {
	switch rootlessCacheRisk(os.Geteuid(), os.Getenv) {
	case rootlessRiskNone:
	case rootlessRiskNoRedirection:
		slog.Warn("running as a non-default UID with no tool-cache redirection; "+
			rootlessCacheConsequence,
			"uid", os.Geteuid(),
			"home", os.Getenv("HOME"),
			"fix", "run as the image's default UID 12021, or set RENOVATE_BINARY_SOURCE=global "+
				"and redirect each tool cache to a writable volume, forwarding it to Renovate's "+
				"artifact subprocesses via RENOVATE_CUSTOM_ENV_VARIABLES (or a config.js "+
				"customEnvVariables) — see the README, 'Running as a non-default user'")
	case rootlessRiskNoCacheVars:
		slog.Warn("running as a non-default UID and RENOVATE_CUSTOM_ENV_VARIABLES "+
			"redirects no tool cache; "+rootlessCacheConsequence,
			"uid", os.Geteuid(),
			"home", os.Getenv("HOME"),
			"custom_env_vars", strings.Join(customEnvVarNames(os.Getenv("RENOVATE_CUSTOM_ENV_VARIABLES")), ","),
			"fix", "add each redirected tool-cache variable (GOCACHE, npm_config_cache, …) "+
				"to RENOVATE_CUSTOM_ENV_VARIABLES so it reaches Renovate's artifact "+
				"subprocesses — see the README, 'Running as a non-default user'")
	}
}

// rootlessCacheConsequence is the shared consequence clause of both warnings:
// what actually breaks when the tool caches stay unredirected.
const rootlessCacheConsequence = "a custom UID has no writable containerbase home, " +
	"so artifact/lockfile regeneration (go.sum, package-lock.json) will likely fail " +
	"and dependency PRs will be raised with stale lockfiles that break the consuming repo's CI"

// rootlessCacheRisk is the pure decision (see the section comment for the
// strategy and its line in the sand). getenv is injected so tests exercise
// the matrix without changing the real UID or environment. No filesystem
// probe: a write-probe of $HOME would be redundant with the UID check and a
// needless side effect.
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

// customEnvVarNames is the single parse: the JSON object's key names, sorted
// for deterministic logs. Undecodable or non-object input yields no names —
// it redirects no cache, and Renovate itself fails loudly on it at startup.
// Values are deliberately never decoded (see the section comment).
func customEnvVarNames(raw string) []string {
	var vars map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &vars); err != nil {
		return nil
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// cacheLikeEnvVar reports whether a variable name plausibly redirects a tool
// cache or toolchain home: any name containing "cache" (GOCACHE,
// npm_config_cache, PIP_CACHE_DIR, YARN_CACHE_FOLDER, …) plus the well-known
// path/home variables that don't. The namespace is unbounded across language
// managers, so this stays an open-ended heuristic rather than a maintained
// allowlist; its error directions are the accepted ones in the section comment.
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
