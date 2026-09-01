package main

import (
	"slices"
	"strings"
	"testing"
)

// Append the override last because os/exec keeps the last duplicate key.
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
		override,
		key,
		key + "X=1",
		"dumb_init_setsid=1",
		prefix + "1\x00" + key + "X=1",
		prefix + "\x00" + prefix + "00",
		"PATH=/usr/bin\x00\x00PATH=/usr/bin",
		"\xff\xfe=/invalid-utf8-key",
		prefix + "0\x01",
		"",
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
