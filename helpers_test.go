package main

import (
	"context"
	"os/exec"

	scheduler "github.com/cplieger/scheduler/v2"
)

// shellAssertRunner returns a CommandRunner whose child is a shell running
// script — exit 0 iff the assertion holds — used to verify the child's real
// environment.
func shellAssertRunner(script string) scheduler.CommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}
