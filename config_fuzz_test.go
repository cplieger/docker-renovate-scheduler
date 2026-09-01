package main

import (
	"strings"
	"testing"
)

// NUL-joined entries make the target independent of the ambient environment.
func FuzzBaseDirForEnvLastWinsNonEmpty(f *testing.F) {
	const key = "RENOVATE_BASE_DIR"

	seeds := []string{
		key + "=/from-client",
		"PATH=/usr/bin",
		key + "=",
		key + "=/first\x00" + key + "=/second",
		key + "=/second\x00" + key + "=",
		key,
		key + "_EXTRA=/x",
		"renovate_base_dir=/lower",
		key + "=/a=b",
		"=/orphan",
		"=",
		key + "=relative/path",
		key + "=../escape",
		key + "=/data\x00PATH=/usr/bin\x00" + key + "=/late",
		"\xff\xfe=/invalid-utf8-key",
		key + "=/nul\x01in\x02value",
		"", // a single empty entry
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, joined string) {
		env := strings.Split(joined, "\x00")

		got := baseDirForEnv(env)
		if got == "" {
			t.Fatalf("baseDirForEnv(%q) = \"\", want a non-empty path: an empty base dir makes the preflight probe nothing and admits the run anyway", env)
		}

		want := defaultBaseDir
		for _, kv := range env {
			k, value, ok := strings.Cut(kv, "=")
			if !ok || k != key {
				continue
			}
			if value == "" {
				want = defaultBaseDir
				continue
			}
			want = value
		}
		if got != want {
			t.Fatalf("baseDirForEnv(%q) = %q, want %q (last %s entry wins, matching the environment the child will actually receive)", env, got, want, key)
		}
	})
}
