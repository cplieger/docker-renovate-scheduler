package main

import (
	"slices"
	"strings"
	"testing"
)

// Append the overrides last because os/exec keeps the last duplicate key.
func FuzzWithChildOverridesOverridesAreLast(f *testing.F) {
	const (
		setsidKey      = "DUMB_INIT_SETSID"
		setsidPrefix   = setsidKey + "="
		nodeArgsKey    = "RENOVATE_NODE_ARGS"
		nodeArgsPrefix = nodeArgsKey + "="
	)
	overrides := []string{
		setsidPrefix + "0",
		nodeArgsPrefix + "--unhandled-rejections=strict",
	}

	seeds := []string{
		"PATH=/usr/bin\x00RENOVATE_X=y",
		setsidPrefix + "1\x00PATH=/usr/bin",
		setsidPrefix + "1\x00PATH=/usr/bin\x00" + setsidPrefix,
		setsidPrefix + "1\x00" + setsidPrefix + "1",
		overrides[0],
		overrides[1],
		overrides[1] + "\x00" + overrides[0],
		setsidKey,
		nodeArgsKey,
		setsidKey + "X=1",
		nodeArgsKey + "X=1",
		"dumb_init_setsid=1",
		"renovate_node_args=other",
		nodeArgsPrefix + "other\x00PATH=/usr/bin",
		nodeArgsPrefix + "other\x00" + setsidPrefix + "1",
		nodeArgsPrefix + "--unhandled-rejections=warn\x00" + nodeArgsPrefix,
		"NODE_OPTIONS=--max-old-space-size=4096",
		setsidPrefix + "1\x00" + setsidKey + "X=1",
		setsidPrefix + "\x00" + setsidPrefix + "00",
		"PATH=/usr/bin\x00\x00PATH=/usr/bin",
		"\xff\xfe=/invalid-utf8-key",
		setsidPrefix + "0\x01",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, joined string) {
		env := strings.Split(joined, "\x00")

		got := withChildOverrides(env)

		if len(got) < len(overrides) || !slices.Equal(got[len(got)-len(overrides):], overrides) {
			t.Fatalf("withChildOverrides(%q) = %q, want %q last: os/exec keeps the last value for a duplicate key, so any other position lets a forwarded value win", env, got, overrides)
		}
		want := slices.Concat(env, overrides)
		if !slices.Equal(got, want) {
			t.Fatalf("withChildOverrides(%q) = %q, want %q (every forwarded entry preserved in order, then the overrides)", env, got, want)
		}
	})
}
