package main

import (
	"strings"
	"testing"
)

// FuzzBaseDirForEnvLastWinsNonEmpty drives the per-run base-dir resolution
// over a job's FORWARDED environment — the socket client's complete environ,
// JSON-decoded off the wire by the trigger library and handed here unparsed.
// The resolved string is then created, written to and fsynced by the preflight
// (verifyBaseDirAt) and becomes Renovate's own data directory, so this is the
// one place in the app where wire content decides a filesystem path.
//
// Two invariants, both independent of how the scan is written:
//
//  1. The result is NEVER empty. An empty path would make the preflight's
//     atomicfile.ProbeWritable report "not attempted" instead of probing
//     anything, so a run would be admitted against a directory nobody
//     checked. Only the "=" split and the empty-value branch stand between
//     a malformed entry and that outcome.
//  2. Duplicate keys resolve LAST-WINS, matching exec semantics — the same
//     value the child process will see in its own environment. The oracle
//     scans FORWARD and remembers the last match, where production scans
//     backward and takes the first; two directions that must agree.
//
// The interesting inputs are all at the "=" boundary, which is where the
// coverage-guided run earns its keep over the table test: an entry with no
// "=" at all, an entry that is only "=", a key that merely prefixes
// RENOVATE_BASE_DIR, a value containing further "=", and NUL or invalid-UTF-8
// bytes inside either half.
//
// Entries arrive NUL-joined because testing.F cannot fuzz a []string
// directly. strings.Split never returns nil, so env is always non-nil and the
// nil branch (which reads the daemon's own environment) is never taken — that
// keeps the target standalone, with no ambient environment in the fixture.
func FuzzBaseDirForEnvLastWinsNonEmpty(f *testing.F) {
	const key = "RENOVATE_BASE_DIR"

	seeds := []string{
		key + "=/from-client",
		"PATH=/usr/bin",
		key + "=",
		key + "=/first\x00" + key + "=/second",
		key + "=/second\x00" + key + "=", // last entry empty: falls back to the default
		key,                              // no "=" at all
		key + "_EXTRA=/x",                // prefix-adjacent key
		"renovate_base_dir=/lower",       // case-sensitive: not a match
		key + "=/a=b",                    // further "=" inside the value
		"=/orphan",                       // empty key
		"=",                              // only the separator
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
