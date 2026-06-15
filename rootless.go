package main

import (
	"log/slog"
	"os"
)

// defaultImageUID is the non-root user baked into the renovate base image
// (passwd entry `ubuntu:x:12021:0`). Running as this UID — or as root — gives
// containerbase a writable home and tool directory, so Renovate installs
// toolchains and regenerates lockfiles with no extra configuration.
const defaultImageUID = 12021

// warnIfRootlessCacheUnwritable emits a loud startup warning when the container
// is run as a non-default UID whose HOME is unwritable and which has not
// redirected its tool caches. That combination silently breaks Renovate's
// artifact/lockfile regeneration (go.sum, package-lock.json): the dependency PR
// is still raised but updates the manifest only, then fails the consuming
// repo's CI days later. Surfacing it at startup turns that delayed, cross-repo
// failure into an immediate, local signal. Advisory only — it never blocks
// startup.
func warnIfRootlessCacheUnwritable() {
	if !rootlessCacheLikelyUnwritable(os.Geteuid(), os.Getenv("HOME"), os.Getenv, isWritableDir) {
		return
	}
	slog.Warn("running as a non-default UID with an unwritable HOME; Renovate's "+
		"language-tool caches default under $HOME, so artifact/lockfile regeneration "+
		"(go.sum, package-lock.json) will fail and dependency PRs will be raised with "+
		"stale lockfiles that break the consuming repo's CI",
		"uid", os.Geteuid(),
		"home", os.Getenv("HOME"),
		"fix", "run as the image's default UID 12021, or set RENOVATE_BINARY_SOURCE=global "+
			"and redirect each tool cache to a writable volume — see the README, "+
			"'Running as a non-default user'")
}

// rootlessCacheLikelyUnwritable is the pure decision behind the warning, split
// out for testing. It returns true only for a non-default, non-root UID whose
// HOME is not a writable directory and which has not redirected its tool caches
// (the documented mitigation). getenv and writableDir are injected so tests can
// exercise the matrix without changing the real UID, HOME, or filesystem.
func rootlessCacheLikelyUnwritable(
	euid int,
	home string,
	getenv func(string) string,
	writableDir func(string) bool,
) bool {
	// Default user or root: containerbase's home and tool dir are writable.
	if euid == defaultImageUID || euid == 0 {
		return false
	}
	// A writable HOME means the default cache locations ($HOME/.npm, etc.) work.
	if writableDir(home) {
		return false
	}
	// Treat explicit cache redirection as mitigation. A custom-UID operator who
	// applied the documented recipe has HOME=/ but working caches; don't nag them.
	if getenv("RENOVATE_CUSTOM_ENV_VARIABLES") != "" ||
		getenv("GOCACHE") != "" ||
		getenv("npm_config_cache") != "" {
		return false
	}
	return true
}

// isWritableDir reports whether path is a directory the current process can
// create files in. It probes by creating and removing a temp file rather than
// reasoning about modes — robust against containerbase's group-0 / owner-only
// permission layout. An empty path or "/" is treated as not writable (the
// signature of a UID with no passwd entry, HOME=/).
func isWritableDir(path string) bool {
	if path == "" || path == "/" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	f, err := os.CreateTemp(path, ".drs-writable-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
