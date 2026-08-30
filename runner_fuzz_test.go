package main

import (
	"slices"
	"strings"
	"testing"
)

// FuzzWithDumbInitInGroupOverrideIsLast drives the containment override over a
// job's FORWARDED environment — the socket client's complete environ, which
// becomes the Renovate child's cmd.Env verbatim. DUMB_INIT_SETSID=0 is what
// keeps the nested per-run dumb-init from forking Renovate into a new session
// below the scheduler's Setpgid group; both kill-escalation stages address only
// -cmd.Process.Pid, so a session-escaped tree survives them and keeps writing
// the base dir under the next queued job.
//
// os/exec keeps the LAST value for a duplicate key when it builds the child's
// environ, so the property is positional: the override is appended last and
// nothing the client forwarded is dropped, reordered or rewritten. Two
// invariants:
//
//  1. The last entry is exactly the override, whatever the input carried —
//     including a forwarded DUMB_INIT_SETSID=1, an entry with no "=", the
//     prefix-adjacent DUMB_INIT_SETSIDX=1 and the lowercase spelling, none of
//     which the function may treat specially.
//  2. Every input entry survives, in input order, with its multiplicity. A
//     re-introduced filter, a sort or a rewrite goes red here.
//
// Entries arrive NUL-joined because testing.F cannot fuzz a []string
// directly. strings.Split never returns nil, so env is always non-nil and the
// os.Environ() branch is never taken — that keeps the target standalone, with
// no ambient environment leaking into the fixture.
func FuzzWithDumbInitInGroupOverrideIsLast(f *testing.F) {
	const (
		key      = "DUMB_INIT_SETSID"
		prefix   = key + "="
		override = prefix + "0"
	)

	seeds := []string{
		"PATH=/usr/bin\x00RENOVATE_X=y",
		prefix + "1\x00PATH=/usr/bin",
		prefix + "1\x00PATH=/usr/bin\x00" + prefix,
		prefix + "1\x00" + prefix + "1",
		override,                             // already correct: still re-appended
		key,                                  // no "=": kept as any other entry
		key + "X=1",                          // prefix-adjacent key: kept
		"dumb_init_setsid=1",                 // lowercase spelling: kept
		prefix + "1\x00" + key + "X=1",       // the pair that separates the two prefixes
		prefix + "\x00" + prefix + "00",      // empty and multi-digit values
		"PATH=/usr/bin\x00\x00PATH=/usr/bin", // duplicate survivor plus an empty entry
		"\xff\xfe=/invalid-utf8-key",
		prefix + "0\x01",
		"", // a single empty entry
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, joined string) {
		env := strings.Split(joined, "\x00")

		got := withDumbInitInGroup(env)

		if len(got) == 0 || got[len(got)-1] != override {
			t.Fatalf("withDumbInitInGroup(%q) = %q, want %q last: os/exec keeps the last value for a duplicate key, so any other position lets a forwarded value win inside dumb-init", env, got, override)
		}
		want := append(slices.Clone(env), override)
		if !slices.Equal(got, want) {
			t.Fatalf("withDumbInitInGroup(%q) = %q, want %q (every forwarded entry preserved in order, then the override)", env, got, want)
		}
	})
}
