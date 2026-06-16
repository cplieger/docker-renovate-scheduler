package main

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"
)

// triggerCapture is a slog.Handler that records, in invocation order, the
// "trigger" attribute of every "renovate run starting" record and the "rerun"
// attribute of every "trigger arrived during run; coalescing rerun" record.
// The scheduler is logs-only (it emits no metrics and exposes no API), so
// these structured log lines are its observable output: the per-pass trigger
// label proves which iteration of the coalescing loop produced the pass, and
// the rerun counter proves the value the loop logs. Asserting on them lets a
// test pin the coalescing branch (reruns > 0) and the counter arithmetic
// (reruns+1) through behaviour rather than internal state.
type triggerCapture struct {
	starts []string // trigger= of each "renovate run starting", in order
	reruns []int64  // rerun= of each "coalescing rerun", in order
}

func (c *triggerCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *triggerCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *triggerCapture) WithGroup(string) slog.Handler            { return c }

func (c *triggerCapture) Handle(_ context.Context, r slog.Record) error {
	switch r.Message {
	case "renovate run starting":
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "trigger" {
				c.starts = append(c.starts, a.Value.String())
				return false
			}
			return true
		})
	case "trigger arrived during run; coalescing rerun":
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "rerun" {
				c.reruns = append(c.reruns, a.Value.Int64())
				return false
			}
			return true
		})
	}
	return nil
}

// swapLogger installs h as the default slog handler and returns a restore
// func. Callers must not run in parallel: slog.Default is process-global.
func swapLogger(h slog.Handler) (restore func()) {
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	return func() { slog.SetDefault(prev) }
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
	clearRerunPending(rerunFlagPath)

	captured := &triggerCapture{}
	t.Cleanup(swapLogger(captured))
	calls := 0

	// when
	ok := runRenovatePass(context.Background(), time.Minute, "tick", nil, flagMarkingRunner("true", &calls, 1))

	// then
	if !ok {
		t.Fatal("runRenovatePass() = false, want true (initial pass + one coalesced rerun both succeed)")
	}
	wantStarts := []string{"tick", "tick+rerun"}
	if !slices.Equal(captured.starts, wantStarts) {
		t.Errorf("logged start triggers = %v, want %v (only reruns carry the +rerun suffix; the first pass is the bare trigger)", captured.starts, wantStarts)
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
	clearRerunPending(rerunFlagPath)

	captured := &triggerCapture{}
	t.Cleanup(swapLogger(captured))
	calls := 0

	// when
	ok := runRenovatePass(context.Background(), time.Minute, "tick", nil, flagMarkingRunner("true", &calls, 1))

	// then
	if !ok {
		t.Fatal("runRenovatePass() = false, want true")
	}
	wantReruns := []int64{1}
	if !slices.Equal(captured.reruns, wantReruns) {
		t.Errorf("logged coalescing rerun counters = %v, want %v (the first coalesced rerun is numbered 1, i.e. reruns+1 with reruns=0)", captured.reruns, wantReruns)
	}
}
