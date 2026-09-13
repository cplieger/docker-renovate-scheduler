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
		`{"a":1e999}`,
		`{"GOCACHE":"/g","OTHER":-1e999}`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		got := customEnvVarNames(raw)

		var decoded map[string]any
		// A decode error with nothing decoded means the oracle refused the
		// input outright (bad syntax, or a non-object), so the parser must
		// have too. An error with keys present means the input IS an object
		// the `any` oracle cannot REPRESENT - a number outside float64 -
		// which the value-blind parser accepts by design, so the key set
		// below still applies.
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil && len(decoded) == 0 {
			if len(got) != 0 {
				t.Fatalf("customEnvVarNames(%q) = %q, want no names: an independent decode rejected the input (%v)", raw, got, err)
			}
			return
		}

		// want is sorted and duplicate-free by construction, so the comparison pins both:
		// the names are joined into one log attribute and map order is randomized.
		want := slices.Sorted(maps.Keys(decoded))
		if !slices.Equal(got, want) {
			t.Fatalf("customEnvVarNames(%q) = %q, want %q (exactly the decoded object's key set; anything else means a value reached the log attribute)", raw, got, want)
		}
	})
}
