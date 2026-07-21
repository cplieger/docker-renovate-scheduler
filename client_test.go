package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunClient_ExitCodesOverRealSocket pins the trigger contract end-to-end
// (the same `run [repo …]` → exit 0/1 surface Ofelia and the Komodo action
// consume): a clean run exits 0, a failing run exits 1.
func TestRunClient_ExitCodesOverRealSocket(t *testing.T) {
	tests := []struct {
		name string
		bin  string
		want int
	}{
		{"clean run exits zero", "true", 0},
		{"failed run exits one", "false", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock, _ := startTestServer(t, recordingRunner(tt.bin, nil))
			if code := runClient(sock, []string{"owner/repo"}); code != tt.want {
				t.Errorf("runClient() = %d, want %d", code, tt.want)
			}
		})
	}
}

// TestRunClient_DaemonUnreachableExitsOne pins the no-daemon failure mode:
// an immediate exit 1 (the trigger reports a failed job), never a hang.
// Exit code only: runClient installs the production logger (setupLogger), so
// its output goes to the real stderr, not a capturable test handler — the
// same reason the old suite asserted runRun by exit code alone.
func TestRunClient_DaemonUnreachableExitsOne(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	sock := filepath.Join(t.TempDir(), "absent.sock")
	if code := runClient(sock, nil); code != 1 {
		t.Errorf("runClient() = %d with no daemon, want 1", code)
	}
}

// TestAwaitResult_StreamHandling pins the client's event-stream tolerance
// half of the trigger contract: an unrecognized event is ignored (forward
// compatibility, never fatal), while a stream that ends before the done event
// — the daemon died or was stopped mid-run — exits 1 so the trigger reports a
// failed job instead of a false success.
func TestAwaitResult_StreamHandling(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   int
	}{
		{
			name:   "unknown events are ignored, not fatal",
			stream: `{"event":"queued"}` + "\n" + `{"event":"future-extension"}` + "\n" + `{"event":"done","ok":true}` + "\n",
			want:   0,
		},
		{
			name:   "stream truncated before done exits one",
			stream: `{"event":"queued"}` + "\n" + `{"event":"started"}` + "\n",
			want:   1,
		},
		{
			name:   "immediate EOF exits one",
			stream: "",
			want:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := awaitResult(strings.NewReader(tt.stream), nil); got != tt.want {
				t.Errorf("awaitResult(%q) = %d, want %d", tt.stream, got, tt.want)
			}
		})
	}
}

// TestRunClient_ForwardsItsEnvironment pins the env half of the trigger
// contract at the runClient level: the client forwards its own process
// environment, so a `docker exec -e RENOVATE_X=…` override reaches the
// daemon-spawned child. The fake child asserts the marker variable.
func TestRunClient_ForwardsItsEnvironment(t *testing.T) {
	t.Setenv("RENOVATE_TEST_MARKER", "exec-override")
	runner := shellAssertRunner(`[ "$RENOVATE_TEST_MARKER" = "exec-override" ]`)
	sock, _ := startTestServer(t, runner)
	if code := runClient(sock, nil); code != 0 {
		t.Error("runClient() != 0: the client's environment did not reach the child")
	}
}
