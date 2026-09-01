package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
)

// defaultImageUID is Renovate's non-root image user.
const defaultImageUID = 12021

type rootlessRisk int

const (
	rootlessRiskNone rootlessRisk = iota
	rootlessRiskNoRedirection
	rootlessRiskNoCacheVars
)

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

const rootlessCacheConsequence = "a custom UID has no writable containerbase home, " +
	"so artifact/lockfile regeneration (go.sum, package-lock.json) will likely fail " +
	"and dependency PRs will be raised with stale lockfiles that renovate flags with a red renovate/artifacts check"

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

// customEnvVarNames returns names only; values may contain credentials.
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

// cacheLikeEnvVar is intentionally open-ended for language-manager cache names.
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
