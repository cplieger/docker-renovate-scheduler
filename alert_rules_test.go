package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// logqlRule is one rule of alerts/logql.yaml, read as text: the repo carries
// no YAML parser and the file's shape is fixed by its own header.
type logqlRule struct {
	labels    map[string]string
	name      string
	expr      string
	filter    string
	field     string
	groupBy   string
	window    time.Duration
	threshold float64
}

var (
	lineFilterRe  = regexp.MustCompile("\\|= `([^`]+)`")
	unwrapRe      = regexp.MustCompile(`\| unwrap (\w+)`)
	labelFilterRe = regexp.MustCompile(`\| (\w+)="([^"]*)"`)
	windowRe      = regexp.MustCompile(`\[(\d+)(h|m)\]`)
	groupByRe     = regexp.MustCompile(`\) by \(([^)]*)\)`)
	equalsRe      = regexp.MustCompile(`== (\d+)\s*$`)
)

func readLogQLRules(t *testing.T) []logqlRule {
	t.Helper()
	raw, err := os.ReadFile("alerts/logql.yaml")
	if err != nil {
		t.Fatalf("reading the shipped LogQL rules: %v", err)
	}
	var rules []logqlRule
	for _, block := range strings.Split(string(raw), "- alert: ")[1:] {
		name, rest, _ := strings.Cut(block, "\n")
		_, expr, found := strings.Cut(rest, "expr: |\n")
		if !found {
			t.Fatalf("%s has no block expr", name)
		}
		expr, _, _ = strings.Cut(expr, "for:")
		r := logqlRule{name: strings.TrimSpace(name), expr: strings.TrimSpace(expr), labels: map[string]string{}}
		if m := lineFilterRe.FindAllStringSubmatch(r.expr, -1); len(m) == 1 {
			r.filter = m[0][1]
		} else {
			t.Errorf("%s has %d line filters, want 1", r.name, len(m))
		}
		if m := unwrapRe.FindStringSubmatch(r.expr); m != nil {
			r.field = m[1]
		}
		for _, m := range labelFilterRe.FindAllStringSubmatch(r.expr, -1) {
			r.labels[m[1]] = m[2]
		}
		if m := windowRe.FindStringSubmatch(r.expr); m != nil {
			n, _ := strconv.Atoi(m[1])
			r.window = time.Duration(n) * time.Hour
			if m[2] == "m" {
				r.window = time.Duration(n) * time.Minute
			}
		}
		if m := groupByRe.FindStringSubmatch(r.expr); m != nil {
			r.groupBy = m[1]
		}
		if m := equalsRe.FindStringSubmatch(r.expr); m != nil {
			r.threshold, _ = strconv.ParseFloat(m[1], 64)
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		t.Fatal("alerts/logql.yaml defines no rule")
	}
	return rules
}

func logqlRuleNamed(t *testing.T, name string) logqlRule {
	t.Helper()
	for _, r := range readLogQLRules(t) {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("alerts/logql.yaml has no %s rule", name)
	return logqlRule{}
}

// completedRecords runs every completed-run path and returns the run finished
// records the real executor wrote.
func completedRecords(t *testing.T) []map[string]string {
	t.Helper()
	var records []map[string]string
	for i := range runPaths {
		if runPaths[i].completed {
			_, recs, _ := runThrough(t, &runPaths[i])
			records = append(records, recs...)
		}
	}
	return records
}

// TestLogQLRulesReadTheRecordsTheDaemonWrites binds every LogQL rule to the
// lines the daemon writes: each selects one of the two records by its
// message, every field a rule unwraps is a number on every run finished
// record, and every label a rule filters on is a field each record carries,
// with a value some record writes. Serial: swaps slog.Default.
func TestLogQLRulesReadTheRecordsTheDaemonWrites(t *testing.T) {
	records := completedRecords(t)
	values := map[string]map[string]bool{}
	for _, rec := range records {
		for k, v := range rec {
			if values[k] == nil {
				values[k] = map[string]bool{}
			}
			values[k][v] = true
		}
	}
	allowed := map[string]bool{`msg="` + runFinishedMsg + `"`: true, `msg="` + daemonStoppedMsg + `"`: true}
	for _, r := range readLogQLRules(t) {
		t.Run(r.name, func(t *testing.T) {
			if !allowed[r.filter] {
				t.Errorf("%s filters on %q, want the run finished or daemon stopped record", r.name, r.filter)
			}
			if r.filter != `msg="`+runFinishedMsg+`"` {
				return
			}
			for _, rec := range records {
				if r.field != "" {
					if _, err := strconv.ParseFloat(rec[r.field], 64); err != nil {
						t.Errorf("%s unwraps %q, which the %s record carries as %q, not a number", r.name, r.field, rec["result"], rec[r.field])
					}
				}
				for k := range r.labels {
					if _, ok := rec[k]; !ok {
						t.Errorf("%s filters on %q, which the %s record lacks", r.name, k, rec["result"])
					}
				}
			}
			for k, v := range r.labels {
				if !values[k][v] {
					t.Errorf("%s filters on %s=%q, which no record writes", r.name, k, v)
				}
			}
		})
	}
}

type timedRecord struct {
	fields map[string]string
	at     time.Duration
}

// lastOK evaluates RenovateRunFailed's range aggregation at `at`: the latest
// record of each scope inside the window, keyed by scope.
func lastOK(t *testing.T, r logqlRule, records []timedRecord, at time.Duration) map[string]float64 {
	t.Helper()
	last := map[string]float64{}
	for _, rec := range records {
		if rec.at > at || at-rec.at >= r.window {
			continue
		}
		v, err := strconv.ParseFloat(rec.fields[r.field], 64)
		if err != nil {
			t.Fatalf("%s: %s = %q: %v", r.name, r.field, rec.fields[r.field], err)
		}
		last[rec.fields["scope"]] = v
	}
	return last
}

func recordWith(scope, result, ok string) map[string]string {
	return map[string]string{"msg": runFinishedMsg, "scope": scope, "result": result, "ok": ok}
}

// TestLogQLRunFailedHoldsUntilTheNextGoodRunOfItsScope is the state rule's
// fixture: a failed run fires it for its scope, a good run of the other scope
// leaves it firing, and the next good run of the same scope resolves it.
func TestLogQLRunFailedHoldsUntilTheNextGoodRunOfItsScope(t *testing.T) {
	r := logqlRuleNamed(t, "RenovateRunFailed")
	if r.field != "ok" || r.threshold != 0 || !strings.Contains(r.expr, "== 0") {
		t.Fatalf("%s does not fire on ok == 0: %s", r.name, r.expr)
	}
	if r.groupBy != "container, scope" || !strings.Contains(r.expr, "last_over_time(") {
		t.Fatalf("%s is not last_over_time grouped by container and scope: %s", r.name, r.expr)
	}
	firing := func(last map[string]float64, scope string) bool {
		v, ok := last[scope]
		return ok && v == r.threshold
	}
	failed := recordWith("full", resultFailed, "0")
	recs := []timedRecord{{fields: failed}}
	if !firing(lastOK(t, r, recs, time.Hour), "full") {
		t.Error("does not fire one hour after a failed full run")
	}
	recs = append(recs, timedRecord{fields: recordWith("scoped", resultComplete, "1"), at: time.Hour})
	if !firing(lastOK(t, r, recs, 2*time.Hour), "full") {
		t.Error("a good scoped run resolved a failed full run")
	}
	recs = append(recs, timedRecord{fields: recordWith("full", resultRepositoryError, "1"), at: 2 * time.Hour})
	if firing(lastOK(t, r, recs, 3*time.Hour), "full") {
		t.Error("still fires after a tolerated full run, which the run client reports as ok")
	}
	recs = append(recs, timedRecord{fields: recordWith("full", resultComplete, "1"), at: 3 * time.Hour})
	if firing(lastOK(t, r, recs, 4*time.Hour), "full") {
		t.Error("still fires after a good full run followed inside the window")
	}
}

// TestLogQLNoRecentRunCountsOnlyCompleteFullRuns pins the heartbeat: only a
// complete full run keeps it quiet, never a scoped run or a tolerated one.
func TestLogQLNoRecentRunCountsOnlyCompleteFullRuns(t *testing.T) {
	r := logqlRuleNamed(t, "RenovateNoRecentRun")
	if !strings.Contains(r.expr, "absent_over_time(") {
		t.Fatalf("%s is not an absent_over_time rule: %s", r.name, r.expr)
	}
	counts := func(rec map[string]string) bool {
		for k, v := range r.labels {
			if rec[k] != v {
				return false
			}
		}
		return true
	}
	cases := []struct {
		rec  map[string]string
		want bool
	}{
		{recordWith("full", resultComplete, "1"), true},
		{recordWith("scoped", resultComplete, "1"), false},
		{recordWith("full", resultRepositoryError, "1"), false},
		{recordWith("full", resultFailed, "0"), false},
	}
	for _, c := range cases {
		if got := counts(c.rec); got != c.want {
			t.Errorf("a %s %s record keeps the rule quiet = %v, want %v", c.rec["scope"], c.rec["result"], got, c.want)
		}
	}
}

// TestLogQLWindowsAreTwiceTheDefaultRunCycle keeps the state rule's range equal
// to the heartbeat's, so a failed run is still the latest record when the
// heartbeat takes over, at twice the sum of RUN_INTERVAL and RUN_TIMEOUT.
func TestLogQLWindowsAreTwiceTheDefaultRunCycle(t *testing.T) {
	want := 2 * (defaultInterval + defaultRunTimeout)
	for _, name := range []string{"RenovateRunFailed", "RenovateNoRecentRun"} {
		if got := logqlRuleNamed(t, name).window; got != want {
			t.Errorf("%s ranges over %s, want %s", name, got, want)
		}
	}
}
