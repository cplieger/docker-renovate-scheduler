package main

import (
	"path/filepath"
	"testing"
)

func TestRootlessCacheLikelyUnwritable(t *testing.T) {
	const customUID = 568

	// writableDir stub: report the given paths as writable.
	writable := func(paths ...string) func(string) bool {
		set := make(map[string]bool, len(paths))
		for _, p := range paths {
			set[p] = true
		}
		return func(p string) bool { return set[p] }
	}
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	noEnv := env(nil)
	noneWritable := writable()

	tests := []struct {
		name        string
		euid        int
		home        string
		getenv      func(string) string
		writableDir func(string) bool
		want        bool
	}{
		{
			name: "default image UID never warns",
			euid: defaultImageUID, home: "/", getenv: noEnv, writableDir: noneWritable,
			want: false,
		},
		{
			name: "root never warns",
			euid: 0, home: "/", getenv: noEnv, writableDir: noneWritable,
			want: false,
		},
		{
			name: "custom UID with writable HOME does not warn",
			euid: customUID, home: "/home/ubuntu", getenv: noEnv, writableDir: writable("/home/ubuntu"),
			want: false,
		},
		{
			name: "custom UID with unwritable HOME and no mitigation warns",
			euid: customUID, home: "/", getenv: noEnv, writableDir: noneWritable,
			want: true,
		},
		{
			name: "custom UID mitigated via GOCACHE does not warn",
			euid: customUID, home: "/", getenv: env(map[string]string{"GOCACHE": "/data/.cache/go-build"}), writableDir: noneWritable,
			want: false,
		},
		{
			name: "custom UID mitigated via npm_config_cache does not warn",
			euid: customUID, home: "/", getenv: env(map[string]string{"npm_config_cache": "/data/.npm"}), writableDir: noneWritable,
			want: false,
		},
		{
			name: "custom UID mitigated via RENOVATE_CUSTOM_ENV_VARIABLES does not warn",
			euid: customUID, home: "/", getenv: env(map[string]string{"RENOVATE_CUSTOM_ENV_VARIABLES": `{"GOCACHE":"/data/.cache/go-build"}`}), writableDir: noneWritable,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rootlessCacheLikelyUnwritable(tc.euid, tc.home, tc.getenv, tc.writableDir)
			if got != tc.want {
				t.Errorf("rootlessCacheLikelyUnwritable(euid=%d, home=%q) = %v, want %v",
					tc.euid, tc.home, got, tc.want)
			}
		})
	}
}

func TestIsWritableDir(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "writable temp dir", path: dir, want: true},
		{name: "empty path", path: "", want: false},
		{name: "root", path: "/", want: false},
		{name: "nonexistent path", path: filepath.Join(dir, "does-not-exist"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWritableDir(tc.path); got != tc.want {
				t.Errorf("isWritableDir(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
