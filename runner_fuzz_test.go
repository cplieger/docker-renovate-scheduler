package main

import (
	"slices"
	"strings"
	"testing"
)

// FuzzWithDumbInitInGroupExactlyOneOverride drives the containment override
// over a job's FORWARDED environment — the socket client's complete environ,
// which becomes the Renovate child's cmd.Env verbatim. DUMB_INIT_SETSID=0 is
// what keeps the nested per-run dumb-init from forking Renovate into a new
// session below the scheduler's Setpgid group; both kill-escalation stages
// address only -cmd.Process.Pid, so a session-escaped tree survives them and
// keeps writing the base dir under the next queued job.
//
// dumb-init is a C program whose getenv returns the FIRST environ match, so
// the property is not "the override is present" but "the override is the ONLY
// entry with that key". Three invariants, the first two stated independently
// of how the filter is written:
//
//  1. Exactly one output entry carries the DUMB_INIT_SETSID= prefix, and its
//     value is 0. A forwarded =1 that merely gets appended after would win
//     inside dumb-init while every shell-level assertion still passed.
//  2. That entry is last, so it is unreachable by any first-match reader
//     other than as the sole match.
//  3. Every other entry survives, in input order, with its multiplicity. The
//     oracle re-derives the filter, which is what makes the prefix BOUNDARY
//     the thing under test: DUMB_INIT_SETSID with no "=" must be kept, and so
//     must DUMB_INIT_SETSIDX=1 and the lowercase spelling. A regression that
//     dropped the "=" from the prefix passes every case a table enumerates
//     and fails here.
//
// Entries arrive NUL-joined because testing.F cannot fuzz a []string
// directly. strings.Split never returns nil, so env is always non-nil and the
// os.Environ() branch is never taken — that keeps the target standalone, with
// no ambient environment leaking into the fixture.
func FuzzWithDumbInitInGroupExactlyOneOverride(f *testing.F) {
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
		override,                             // already correct: still dropped, then re-appended
		key,                                  // no "=": must be KEPT
		key + "X=1",                          // prefix-adjacent key: must be KEPT
		"dumb_init_setsid=1",                 // case-sensitive: must be KEPT
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

		matches := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, prefix) {
				matches++
				if kv != override {
					t.Fatalf("withDumbInitInGroup(%q) emitted %q, want %q: dumb-init's getenv takes the first match, so any other value reopens the session escape", env, kv, override)
				}
			}
		}
		if matches != 1 {
			t.Fatalf("withDumbInitInGroup(%q) = %q with %d %s entries, want exactly 1", env, got, matches, prefix)
		}
		if got[len(got)-1] != override {
			t.Fatalf("withDumbInitInGroup(%q) = %q, want %q last", env, got, override)
		}

		want := make([]string, 0, len(env)+1)
		for _, kv := range env {
			if !strings.HasPrefix(kv, prefix) {
				want = append(want, kv)
			}
		}
		want = append(want, override)
		if !slices.Equal(got, want) {
			t.Fatalf("withDumbInitInGroup(%q) = %q, want %q (every non-%s entry preserved in order, then the override)", env, got, want, prefix)
		}
	})
}
