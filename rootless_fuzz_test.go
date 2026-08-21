package main

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// FuzzCustomEnvVarNamesMatchesDecodedKeys drives the app's only
// json.Unmarshal of external data: RENOVATE_CUSTOM_ENV_VARIABLES, an
// operator-supplied string the boot classifier parses and the soft warning
// joins into one log attribute.
//
// The oracle is an independent decode into map[string]any. That walks a
// different value path than production's map[string]json.RawMessage, so
// agreement on BOTH the decode verdict and the resulting key set is a
// cross-check rather than a restatement of the parser. Four invariants:
//
//  1. It never panics, whatever the input.
//  2. The output is exactly the decoded object's key set — nothing invented,
//     nothing dropped. This is the security property the file's section
//     comment states as "key NAMES only": a forwarded VALUE can hold a
//     credential (an HTTP_PROXY with basic auth) and must never reach the
//     log, so a returned name that is not a key of the object is a leak.
//  3. The output is sorted, which is what keeps the operator-facing warning
//     deterministic across Go's randomized map iteration order.
//  4. The output carries no duplicate name, so the joined attribute cannot
//     repeat a variable.
//
// Undecodable and non-object input must both yield NO names. The assertion
// is on LENGTH, not nil-ness, because the two paths genuinely differ — an
// unmarshal error returns nil while a decoded `null` returns an empty
// non-nil slice — and both consumers (slices.ContainsFunc, strings.Join)
// are nil-safe, so length is the property that matters.
func FuzzCustomEnvVarNamesMatchesDecodedKeys(f *testing.F) {
	seeds := []string{
		`{"GOCACHE":"/data/.cache/go-build"}`,
		`{"npm_config_cache":"/n","GOCACHE":"/g","HTTP_PROXY":"http://user:pass@proxy"}`,
		`{"GOPATH":"/data/go"}`,
		`{}`,
		`{"GOCACHE":"/a","GOCACHE":"/b"}`, // duplicate key, last wins into one entry
		`{"":"x"}`,                        // empty key name
		`{"\u0047OCACHE":"/g"}`,           // escaped key, decodes to GOCACHE
		`{"\ud800":"x"}`,                  // lone surrogate, replaced with U+FFFD
		`{"GOCACHE":{"nested":[1,2,3]}}`,  // non-string value
		`{"HOME":null}`,
		`{"a":1,"A":2}`,     // case-distinct keys are distinct names
		`{"GOCACHE":"/g",}`, // trailing comma: invalid JSON
		`["GOCACHE"]`,       // non-object JSON
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
