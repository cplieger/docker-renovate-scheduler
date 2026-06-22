package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/health"
)

// --- Main ---

// main dispatches on the first argument: `health` runs the Docker probe,
// `run` runs one Renovate pass and exits (the external-trigger entry point;
// any further arguments are passed through to Renovate as repository slugs),
// and anything else (including no argument) runs the long-lived daemon.
func main() {
	// CLI health probe for the Docker healthcheck. Checked before the logger
	// is configured because RunProbe calls os.Exit.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(healthMarkerPath)
	}

	cmd := "daemon"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "daemon":
		if err := run(context.Background()); err != nil {
			os.Exit(1)
		}
	case "run":
		os.Exit(runRun(context.Background(), os.Args[2:]))
	default:
		setupLogger()
		slog.Error("unknown subcommand", "command", cmd, "valid", "daemon, run, health")
		os.Exit(2)
	}
}

// run is the composition root for the long-running container (the `daemon`
// subcommand and the default no-arg command). It configures logging,
// verifies the Renovate base directory is writable, wires the health marker,
// and dispatches to the built-in interval scheduler or the idle
// external-trigger loop based on SCHED_INTERVAL. Returning an error exits
// non-zero.
func run(ctx context.Context) error {
	setupLogger()
	warnIfRootlessCacheUnwritable()

	if err := verifyBaseDir(ctx); err != nil {
		slog.Error("base directory not writable", "path", baseDir(), "error", err,
			"hint", "mount a writable volume at RENOVATE_BASE_DIR (the image default is /data); a read_only container needs a /data volume or tmpfs")
		return err
	}

	interval, scheduleEnabled := loadInterval()
	timeout := loadRunTimeout()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	marker := health.NewMarker(healthMarkerPath)
	defer marker.Cleanup()

	if scheduleEnabled {
		runBuiltin(ctx, marker, interval, timeout)
		return nil
	}
	runExternal(ctx, marker, timeout)
	return nil
}

// runBuiltin runs the self-contained interval scheduler: a startup run that
// fires immediately for freshness on deploy, plus a ticker loop that fires
// every interval. The flock in runRenovatePass guards against overlap if a
// run takes longer than the interval. Both goroutines share the wait group
// so shutdown waits for in-flight work. Each run sets the health marker
// from its outcome.
func runBuiltin(ctx context.Context, marker *health.Marker, interval, timeout time.Duration) {
	// Remove any stale marker from a previous run that may have crashed
	// before its defer ran. The first run flips it to its real value.
	marker.Set(false)

	slog.Info("container started (built-in scheduling)",
		"interval", interval, "timeout", timeout, "base_dir", baseDir())

	var wg sync.WaitGroup
	wg.Go(func() {
		marker.Set(runRenovatePass(ctx, timeout, "startup", nil, defaultCommandRunner))
	})
	wg.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				marker.Set(runRenovatePass(ctx, timeout, "interval", nil, defaultCommandRunner))
			}
		}
	})

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Mark unhealthy immediately so observers see the signal before the run
	// drain (a Renovate run can take a while).
	marker.Set(false)

	// Wait for the startup run and any in-flight ticker run to drain.
	wg.Wait()
}

// runExternal idles until shutdown. The built-in scheduler is disabled
// (SCHED_INTERVAL=off); runs are triggered out-of-band via the `run`
// subcommand (e.g. an Ofelia job-exec on a cron schedule, or a Komodo action
// on a release webhook). The marker is set healthy on boot so an idle,
// not-yet-triggered container reads healthy; each `run` invocation updates
// it on disk.
//
// On shutdown the daemon does not exit immediately: an external `run` may be
// mid-pass (a separate `docker exec` process the daemon can't wait() on), so
// it drains that run via the shared overlap flock before returning. drainTimeout
// (SCHED_TIMEOUT) caps the wait at a single run's own maximum lifetime.
func runExternal(ctx context.Context, marker *health.Marker, drainTimeout time.Duration) {
	marker.Set(true)

	slog.Info("container started (external scheduling)",
		"base_dir", baseDir(), "trigger", "docker-renovate-scheduler run")

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Mark unhealthy immediately so observers see the signal before the run
	// drain (a Renovate run can take a while).
	marker.Set(false)

	// An external `run` (a separate `docker exec` process — an Ofelia job-exec
	// or the Komodo release-trigger action) may be mid-pass when the container
	// is asked to stop, typically a redeploy of the stack landing on top of an
	// in-progress run. PID 1 can't wait() on that separate process, but the run
	// holds the overlap flock, so wait for it to release before exiting. Docker
	// only tears the container down (SIGKILLing the run) once PID 1 exits or
	// stop_grace_period elapses, so draining here lets the run finish with its
	// real exit code instead of being SIGKILLed (exit 137) and reported as a
	// failed job. stop_grace_period is the real outer bound — set it long
	// enough to cover a normal run.
	waitForRunToDrain(lockFilePath, drainPollInterval, drainTimeout)
}

// runRun performs exactly one Renovate pass and returns the process exit
// code: 0 on success, 1 on failure. This is what the external scheduler
// invokes (`docker exec ... docker-renovate-scheduler run [repo ...]`).
// Optional repo arguments are passed straight through to Renovate to
// restrict the run to specific repositories. Unlike the daemon it does not
// clean up the marker on exit — the file must persist so the running
// container's healthcheck reflects this run.
func runRun(ctx context.Context, repoArgs []string) int {
	setupLogger()
	warnIfRootlessCacheUnwritable()

	if err := verifyBaseDir(ctx); err != nil {
		slog.Error("base directory not writable", "path", baseDir(), "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	timeout := loadRunTimeout()
	marker := health.NewMarker(healthMarkerPath)
	if ok := runRenovatePass(ctx, timeout, "external", repoArgs, defaultCommandRunner); !ok {
		marker.Set(false)
		return 1
	}
	marker.Set(true)
	return 0
}

// drainPollInterval is how often the external-mode shutdown drain re-checks
// whether an in-flight run still holds the overlap lock. Runs take minutes, so
// a sub-second poll is cheap and keeps the post-completion shutdown delay
// negligible.
const drainPollInterval = 500 * time.Millisecond

// waitForRunToDrain blocks until no external `run` holds the overlap lock at
// lockPath — the in-flight run finished, or its process died (flock releases on
// process exit) — or maxWait elapses, whichever comes first. It is the
// external-scheduling shutdown drain: PID 1 cannot wait() on the separate `run`
// process, so it polls the shared flock instead. It returns true if the run
// drained and false if maxWait elapsed first (the caller then exits and the
// container stop terminates any lingering run). maxWait is the run's own
// SCHED_TIMEOUT, so the wait can never outlast a single run's maximum lifetime.
func waitForRunToDrain(lockPath string, poll, maxWait time.Duration) bool {
	inFlight, err := runInFlight(lockPath)
	if err != nil {
		slog.Warn("cannot probe the run lock during shutdown; exiting without draining", "error", err)
		return false
	}
	if !inFlight {
		return true
	}

	slog.Info("waiting for in-flight renovate run to finish before shutdown", "max_wait", maxWait)
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.C:
			slog.Warn("in-flight renovate run did not finish within the drain window; exiting (container stop will terminate it)",
				"max_wait", maxWait)
			return false
		case <-ticker.C:
			inFlight, err := runInFlight(lockPath)
			if err != nil {
				slog.Warn("cannot probe the run lock during shutdown; exiting without draining", "error", err)
				return false
			}
			if !inFlight {
				slog.Info("in-flight renovate run finished; proceeding with shutdown")
				return true
			}
		}
	}
}
