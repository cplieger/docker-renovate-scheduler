package main

import (
	"errors"
	"sync"
	"time"
)

// --- Run queue ---
//
// The daemon is the single owner of Renovate execution: every trigger — the
// built-in ticker and each socket client — submits a job here, and one
// executor goroutine (daemon.runJobs) serves them strictly in order. FIFO
// with no coalescing: every accepted request gets its own run and its own
// true result, which is both simpler and more honest than the old
// cross-process rerun coalescing (a queued trigger's scope and environment
// replay exactly; nothing is merged, deferred, or replayed with the wrong
// scope). Renovate runs are idempotent, so back-to-back runs from a trigger
// burst cost only time.

// queueCapacity bounds pending requests. The realistic trigger set is one
// periodic job plus a release-webhook burst, so 16 is generous headroom; a
// client hitting a full queue is rejected immediately with a clear reason
// (honest backpressure) rather than queued unboundedly.
const queueCapacity = 16

var (
	errQueueClosed = errors.New("scheduler is shutting down")
	errQueueFull   = errors.New("run queue is full")
)

// job is one queued run request.
type job struct {
	// started is closed by the executor the moment the run begins.
	started chan struct{}
	// result receives exactly one outcome per accepted job — from the run
	// itself, or from shutdown cancellation. Buffered so the executor never
	// blocks on a departed waiter.
	result chan runOutcome
	// trigger labels the run's origin in logs: startup, interval, external.
	trigger string
	// repos are positional repository slugs; empty means Renovate's own
	// repositories / autodiscover configuration decides the set.
	repos []string
	// env is the complete environment for the Renovate child; nil means
	// inherit the daemon's own environment (ticker-submitted runs).
	env []string
}

// runOutcome is a job's final result.
type runOutcome struct {
	// reason explains a not-ok outcome that isn't a plain Renovate failure
	// (cancelled by shutdown, base directory unwritable).
	reason   string
	duration time.Duration
	ok       bool
}

// newJob builds a job for the given trigger.
func newJob(trigger string, repos, env []string) *job {
	return &job{
		repos:   repos,
		env:     env,
		trigger: trigger,
		started: make(chan struct{}),
		result:  make(chan runOutcome, 1),
	}
}

// finish delivers the job's single result.
func (j *job) finish(out runOutcome) {
	j.result <- out
}

// runQueue is the bounded FIFO between triggers and the executor. Submission
// is non-blocking: a full or closed queue rejects immediately. The channel is
// the queue; the executor is its only receiver.
type runQueue struct {
	jobs   chan *job
	mu     sync.Mutex
	closed bool
}

func newRunQueue(capacity int) *runQueue {
	return &runQueue{jobs: make(chan *job, capacity)}
}

// submit enqueues j, failing fast when the queue is full or the daemon is
// shutting down. An accepted job is guaranteed exactly one result. The send
// is non-blocking and happens under the mutex, so it can never race close's
// channel close.
func (q *runQueue) submit(j *job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errQueueClosed
	}
	select {
	case q.jobs <- j:
		return nil
	default:
		return errQueueFull
	}
}

// close stops admission and closes the channel, letting the executor's range
// loop drain the already-queued jobs (it cancels each once shutdown is
// signalled) and terminate. Idempotent; called at shutdown.
func (q *runQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.jobs)
}
