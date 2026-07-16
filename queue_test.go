package main

import (
	"errors"
	"sync"
	"testing"
)

// TestRunQueue_FIFOOrder pins the queue's core contract: jobs come out in
// submission order (every trigger gets its own run, strictly in order).
func TestRunQueue_FIFOOrder(t *testing.T) {
	t.Parallel()
	q := newRunQueue(4)
	a := newJob("external", []string{"a/a"}, nil)
	b := newJob("external", []string{"b/b"}, nil)
	c := newJob("interval", nil, nil)
	for _, j := range []*job{a, b, c} {
		if err := q.submit(j); err != nil {
			t.Fatalf("submit() = %v, want nil", err)
		}
	}
	q.close()
	var got []*job
	for j := range q.jobs {
		got = append(got, j)
	}
	want := []*job{a, b, c}
	if len(got) != len(want) {
		t.Fatalf("drained %d jobs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("job %d out of order", i)
		}
	}
}

// TestRunQueue_FullRejectsImmediately pins the bounded-backpressure contract:
// a full queue rejects with errQueueFull instead of blocking the trigger.
func TestRunQueue_FullRejectsImmediately(t *testing.T) {
	t.Parallel()
	q := newRunQueue(1)
	if err := q.submit(newJob("external", nil, nil)); err != nil {
		t.Fatalf("first submit() = %v, want nil", err)
	}
	if err := q.submit(newJob("external", nil, nil)); !errors.Is(err, errQueueFull) {
		t.Errorf("submit() on a full queue = %v, want errQueueFull", err)
	}
}

// TestRunQueue_ClosedRejectsSubmissions pins the shutdown-admission contract.
func TestRunQueue_ClosedRejectsSubmissions(t *testing.T) {
	t.Parallel()
	q := newRunQueue(4)
	q.close()
	if err := q.submit(newJob("external", nil, nil)); !errors.Is(err, errQueueClosed) {
		t.Errorf("submit() after close = %v, want errQueueClosed", err)
	}
	q.close() // idempotent: a second close must not panic
}

// TestRunQueue_ConcurrentSubmitAndCloseIsSafe hammers submit against close
// under the race detector: the mutex serializes them, so no send can hit a
// closed channel (the panic the naive design allowed).
func TestRunQueue_ConcurrentSubmitAndCloseIsSafe(t *testing.T) {
	t.Parallel()
	for range 50 {
		q := newRunQueue(2)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				_ = q.submit(newJob("external", nil, nil))
			})
		}
		q.close()
		wg.Wait()
		for range q.jobs { // drain only
		}
	}
}
