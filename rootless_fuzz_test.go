package main

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// Only decoded keys may reach the warning; values can contain credentials.
func FuzzCustomEnvVarNamesMatchesDecodedKeys(f *testing.F) {
	seeds := []string{
		`{"GOCACHE":"/data/.cache/go-build"}`,
		`{"npm_config_cache":"/n","GOCACHE":"/g","HTTP_PROXY":"http://user:pass@proxy"}`,
		`{"GOPATH":"/data/go"}`,
		`{}`,
		`{"GOCACHE":"/a","GOCACHE":"/b"}`,
		`{"":"x"}`,
		`{"\u0047OCACHE":"/g"}`,
		`{"\ud800":"x"}`,
		`{"GOCACHE":{"nested":[1,2,3]}}`,
		`{"HOME":null}`,
		`{"a":1,"A":2}`,
		`{"GOCACHE":"/g",}`,
		`["GOCACHE"]`,
		`null`,
		`5`,
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		got := customEnvVarNames(raw)

		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			// The oracle refused the input, so the parser must have too.
			if len(got) != 0 {
				t.Fatalf("customEnvVarNames(%q) = %q, want no names: an independent decode rejected the input (%v)", raw, got, err)
			}
			return
		}

		want := slices.Sorted(maps.Keys(decoded))
		if !slices.Equal(got, want) {
			t.Fatalf("customEnvVarNames(%q) = %q, want %q (exactly the decoded object's key set; anything else means a value reached the log attribute)", raw, got, want)
		}
		if !slices.IsSorted(got) {
			t.Fatalf("customEnvVarNames(%q) = %q, want sorted: the warning joins these into one log attribute and map iteration order is randomized", raw, got)
		}
		for i := 1; i < len(got); i++ {
			if got[i] == got[i-1] {
				t.Fatalf("customEnvVarNames(%q) = %q, want no duplicate name (%q repeats)", raw, got, got[i])
			}
		}
	})
}
