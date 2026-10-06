package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzGitHubMessage_boundedValidRedacted pins that GitHub's error text, which
// reaches the log, is capped, never carries a split or invalid rune, and never
// carries the request credential even when the server echoes it.
func FuzzGitHubMessage_boundedValidRedacted(f *testing.F) {
	const secret = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiI0MjQyIn0.c2lnbmF0dXJl" // gitleaks:allow (fake JWT, redaction fuzz seed)
	f.Add([]byte(`{"message":"Bad credentials"}`))
	f.Add([]byte(`{"message":"` + strings.Repeat("a", 199) + `é"}`))
	f.Add([]byte(`{"message":"\xff\xfe"}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`{"message":"` + secret + `"}`))
	f.Add([]byte(`{"message":"` + strings.Repeat("a", 180) + secret + `"}`))
	f.Add([]byte(`{"message":"` + secret[:10] + "\xff" + secret[10:] + `"}`))
	f.Add([]byte(`{"message":"` + strings.ReplaceAll(secret, ".", `\u002e`) + `"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		got := githubMessage(body, secret)
		if len(got) > maxGitHubMessage {
			t.Fatalf("githubMessage(%q) is %d bytes, want at most %d", body, len(got), maxGitHubMessage)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("githubMessage(%q) = %q, not valid UTF-8", body, got)
		}
		if strings.Contains(got, secret) {
			t.Fatalf("githubMessage(%q) = %q, which carries the secret", body, got)
		}
	})
}
