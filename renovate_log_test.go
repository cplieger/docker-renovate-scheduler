package main

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// The scheduler is logs-only (it emits no metrics and exposes no API), so its
// structured log lines are its observable output: the per-pass trigger label
// proves which iteration of the coalescing loop produced the pass, and the
// rerun counter proves the value the loop logs. The helpers below extract
// those attrs from a capture.Recorder so a test can pin the coalescing branch
// (reruns > 0) and the counter arithmetic (reruns+1) through behaviour rather
// than internal state.

// messageAttrs collects, in capture order, the named attr's value from every
// record whose Message is exactly msg.
func messageAttrs(rec *capture.Recorder, msg, key string) []slog.Value {
	var out []slog.Value
	for _, r := range rec.Records() {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				out = append(out, a.Value)
				return false
			}
			return true
		})
	}
	return out
}

// startTriggers returns the trigger= attr of each "renovate run starting"
// record, in order.
func startTriggers(rec *capture.Recorder) []string {
	values := messageAttrs(rec, "renovate run starting", "trigger")
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v.String()
	}
	return out
}

// rerunCounters returns the rerun= attr of each "trigger arrived during run;
// coalescing rerun" record, in order.
func rerunCounters(rec *capture.Recorder) []int64 {
	values := messageAttrs(rec, "trigger arrived during run; coalescing rerun", "rerun")
	out := make([]int64, len(values))
	for i, v := range values {
		out[i] = v.Int64()
	}
	return out
}

// TestRunRenovatePass_CoalescedRerun_LabelsOnlyRerunsWithSuffix verifies the
// coalescing loop tags only the rerun passes with the "+rerun" suffix: the
// initial pass logs the bare trigger, and the single coalesced rerun logs
// trigger+"+rerun". This pins the `reruns > 0` guard on renovate.go:114 — a
// boundary mutation (`>=`) would suffix the very first pass, and a negation
// mutation (`<=`) would invert which passes get the suffix.
func TestRunRenovatePass_CoalescedRerun_LabelsOnlyRerunsWithSuffix(t *testing.T) {
	// given: a holder whose first pass observes a trigger arriving mid-run,
	// so the loop runs an initial pass plus exactly one coalesced rerun.
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	rerunFlag.Clear()

	// Not parallel: capture.Default swaps the global slog default.
	rec := capture.Default(t)
	calls := 0

	// when
	ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "tick", nil, flagMarkingRunner("true", &calls, 1))

	// then
	if !ok {
		t.Fatal("runRenovatePass() = false, want true (initial pass + one coalesced rerun both succeed)")
	}
	wantStarts := []string{"tick", "tick+rerun"}
	if got := startTriggers(rec); !slices.Equal(got, wantStarts) {
		t.Errorf("logged start triggers = %v, want %v (only reruns carry the +rerun suffix; the first pass is the bare trigger)", got, wantStarts)
	}
}

// TestRunRenovatePass_CoalescedRerun_LogsOneBasedRerunCounter verifies the
// coalescing log line numbers the first rerun as #1. This pins the `reruns+1`
// arithmetic on renovate.go:131 — an ARITHMETIC_BASE mutation (`reruns-1`)
// would log -1 for the first coalesced rerun.
func TestRunRenovatePass_CoalescedRerun_LogsOneBasedRerunCounter(t *testing.T) {
	// given: one trigger arrives during the first pass, producing exactly one
	// coalescing-rerun log line.
	t.Cleanup(func() { _ = os.Remove(rerunFlagPath) })
	rerunFlag.Clear()

	// Not parallel: capture.Default swaps the global slog default.
	rec := capture.Default(t)
	calls := 0

	// when
	ok := runRenovatePass(context.Background(), context.Background(), time.Minute, "tick", nil, flagMarkingRunner("true", &calls, 1))

	// then
	if !ok {
		t.Fatal("runRenovatePass() = false, want true")
	}
	wantReruns := []int64{1}
	if got := rerunCounters(rec); !slices.Equal(got, wantReruns) {
		t.Errorf("logged coalescing rerun counters = %v, want %v (the first coalesced rerun is numbered 1, i.e. reruns+1 with reruns=0)", got, wantReruns)
	}
}
