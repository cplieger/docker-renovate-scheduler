package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestDockerfileStampsTheVersion pins the release pipeline's BUILD_VERSION
// build argument into main.version, which an unstamped build reports as "dev".
func TestDockerfileStampsTheVersion(t *testing.T) {
	t.Parallel()
	if version != "dev" {
		t.Errorf("unstamped version = %q, want dev", version)
	}
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	var hasArg, stamps bool
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if f[0] == "ARG" && slices.Contains(f[1:], "BUILD_VERSION=dev") {
			hasArg = true
		}
		if strings.Contains(line, "go build") && strings.Contains(line, "-X main.version=${BUILD_VERSION}") {
			stamps = true
		}
	}
	if !hasArg {
		t.Error("Dockerfile declares no ARG BUILD_VERSION=dev")
	}
	if !stamps {
		t.Error("the go build line does not pass -X main.version=${BUILD_VERSION}")
	}
}
