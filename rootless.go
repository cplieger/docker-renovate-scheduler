package main

import (
	"log/slog"
	"os"
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
func warnIfRootlessCacheUnwritable() {
	if !rootlessCacheLikelyUnwritable(os.Geteuid(), os.Getenv) {
		return
	}
	slog.Warn("running as a non-default UID with no tool-cache redirection; a "+
		"custom UID has no writable containerbase home, so artifact/lockfile "+
		"regeneration (go.sum, package-lock.json) will likely fail and dependency "+
		"PRs will be raised with stale lockfiles that break the consuming repo's CI",
		"uid", os.Geteuid(),
		"home", os.Getenv("HOME"),
		"fix", "run as the image's default UID 12021, or set RENOVATE_BINARY_SOURCE=global "+
			"and redirect each tool cache to a writable volume — see the README, "+
			"'Running as a non-default user'")
}

// rootlessCacheLikelyUnwritable is the pure decision behind the warning, split
// out for testing. It returns true for a non-default, non-root UID that has not
// redirected its tool caches (the documented mitigation). getenv is injected so
// tests can exercise the matrix without changing the real UID or environment.
//
// It deliberately does NOT probe the filesystem. The base image's entrypoint
// sets HOME=/home/ubuntu for every UID, but that directory is writable only by
// the image's own user (12021), so a write-probe of HOME would be both
// redundant with the UID check and a needless side effect. "Non-default UID
// without cache redirection" is the reliable, side-effect-free signal.
func rootlessCacheLikelyUnwritable(euid int, getenv func(string) string) bool {
	if euid == defaultImageUID || euid == 0 {
		return false
	}
	// Explicit cache redirection is the documented custom-UID mitigation; a UID
	// that set it (HOME may still be /) has working caches, so don't nag.
	if getenv("RENOVATE_CUSTOM_ENV_VARIABLES") != "" ||
		getenv("GOCACHE") != "" ||
		getenv("npm_config_cache") != "" {
		return false
	}
	return true
}
