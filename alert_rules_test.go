package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type stageKind int

const (
	stageLineFilter stageKind = iota
	stageLogfmt
	stageLabelFilter
	stageUnwrap
)

// logqlStage is one stage of a rule's log pipeline. key is the text a line
// filter matches, the label a label filter reads or the field unwrap reads.
type logqlStage struct {
	fields []string
	key    string
	value  string
	kind   stageKind
}

// logqlRule is one rule of alerts/logql.yaml, read as text: the repo carries
// no YAML parser and the file's shape is fixed by its own header.
type logqlRule struct {
	stream    map[string]string
	name      string
	expr      string
	groupBy   string
	stages    []logqlStage
	window    time.Duration
	threshold float64
}

var (
	streamRe      = regexp.MustCompile(`\{(\w+)="([^"]*)"\}`)
	labelFilterRe = regexp.MustCompile(`^(\w+)="([^"]*)"$`)
	windowRe      = regexp.MustCompile(`\s*\[(\d+)(h|m)\]`)
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
		rules = append(rules, parseLogQLRule(t, strings.TrimSpace(name), strings.TrimSpace(expr)))
	}
	if len(rules) == 0 {
		t.Fatal("alerts/logql.yaml defines no rule")
	}
	return rules
}

// parseLogQLRule reads the one stream selector, the pipeline up to the range
// and the aggregation around it. A stage the evaluator does not model fails
// the test, so a new rule shape cannot pass unevaluated.
func parseLogQLRule(t *testing.T, name, expr string) logqlRule {
	t.Helper()
	r := logqlRule{name: name, expr: expr, stream: map[string]string{}}
	sel := streamRe.FindAllStringSubmatchIndex(expr, -1)
	win := windowRe.FindStringSubmatchIndex(expr)
	if len(sel) != 1 || win == nil || win[0] < sel[0][1] {
		t.Fatalf("%s is not one selector, a pipeline and a range: %s", name, expr)
	}
	r.stream[expr[sel[0][2]:sel[0][3]]] = expr[sel[0][4]:sel[0][5]]
	n, _ := strconv.Atoi(expr[win[2]:win[3]])
	r.window = time.Duration(n) * time.Hour
	if expr[win[4]:win[5]] == "m" {
		r.window = time.Duration(n) * time.Minute
	}
	for _, piece := range strings.Split(expr[sel[0][1]:win[0]], "|")[1:] {
		r.stages = append(r.stages, parseStage(t, name, strings.TrimSpace(piece)))
	}
	if m := groupByRe.FindStringSubmatch(expr); m != nil {
		r.groupBy = m[1]
	}
	if m := equalsRe.FindStringSubmatch(expr); m != nil {
		r.threshold, _ = strconv.ParseFloat(m[1], 64)
	}
	return r
}

func parseStage(t *testing.T, rule, piece string) logqlStage {
	t.Helper()
	switch {
	case strings.HasPrefix(piece, "= `") && strings.HasSuffix(piece, "`"):
		return logqlStage{kind: stageLineFilter, key: piece[3 : len(piece)-1]}
	case piece == "logfmt":
		return logqlStage{kind: stageLogfmt}
	case strings.HasPrefix(piece, "logfmt "):
		var fields []string
		for f := range strings.SplitSeq(strings.TrimPrefix(piece, "logfmt "), ",") {
			fields = append(fields, strings.TrimSpace(f))
		}
		return logqlStage{kind: stageLogfmt, fields: fields}
	case strings.HasPrefix(piece, "unwrap "):
		return logqlStage{kind: stageUnwrap, key: strings.TrimPrefix(piece, "unwrap ")}
	}
	if m := labelFilterRe.FindStringSubmatch(piece); m != nil {
		return logqlStage{kind: stageLabelFilter, key: m[1], value: m[2]}
	}
	t.Fatalf("%s has a stage the evaluator does not model: %q", rule, piece)
	return logqlStage{}
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

func (r logqlRule) stagesOf(kind stageKind) []logqlStage {
	var out []logqlStage
	for _, s := range r.stages {
		if s.kind == kind {
			out = append(out, s)
		}
	}
	return out
}

func (r logqlRule) unwrapField() string {
	if u := r.stagesOf(stageUnwrap); len(u) == 1 {
		return u[0].key
	}
	return ""
}

var errMisreadShape = errors.New("a logfmt field list ahead of a label filter, which Loki misreads in a metric query (grafana/loki#11334)")

// misreadShape refuses a logfmt field list followed by a label filter. On
// Loki 3.7.9 `absent_over_time` over that pipeline reads absent with matching
// lines in range, and `sum` over its `count_over_time` reads empty, while the
// bare parser reads the same lines correctly.
func misreadShape(r logqlRule) error {
	listed := false
	for _, s := range r.stages {
		switch {
		case s.kind == stageLogfmt && s.fields != nil:
			listed = true
		case s.kind == stageLabelFilter && listed:
			return fmt.Errorf("%s: %w", r.name, errMisreadShape)
		}
	}
	return nil
}

type logSample struct {
	labels map[string]string
	value  string
	kept   bool
}

// eval runs the rule's pipeline over one log line as Loki defines each stage:
// the stream labels first, a line filter on the raw text, logfmt adding every
// field or only the listed ones, a label filter dropping a line whose label
// differs (an absent label reads empty), unwrap reading one label's value.
func (r logqlRule) eval(t *testing.T, line string) logSample {
	t.Helper()
	s := logSample{labels: maps.Clone(r.stream), kept: true}
	for _, st := range r.stages {
		switch st.kind {
		case stageLineFilter:
			if !strings.Contains(line, st.key) {
				return logSample{}
			}
		case stageLogfmt:
			fields := parseLogfmt(t, line)
			if st.fields == nil {
				maps.Copy(s.labels, fields)
			}
			for _, k := range st.fields {
				s.labels[k] = fields[k]
			}
		case stageLabelFilter:
			if s.labels[st.key] != st.value {
				return logSample{}
			}
		case stageUnwrap:
			s.value = s.labels[st.key]
		}
	}
	return s
}

// completedLines runs every completed-run path and returns every line the
// real executor wrote, split into run finished records and the rest.
func completedLines(t *testing.T) (records, others []string) {
	t.Helper()
	for i := range runPaths {
		if !runPaths[i].completed {
			continue
		}
		_, lines, _ := runLinesThrough(t, &runPaths[i])
		for _, l := range lines {
			if l == "" {
				continue
			}
			if parseLogfmt(t, l)["msg"] == runFinishedMsg {
				records = append(records, l)
			} else {
				others = append(others, l)
			}
		}
	}
	return records, others
}

// logqlOracles says which run finished records each rule over them keeps, and
// the value it unwraps, per docs/monitoring.md: RenovateRunFailed reads each
// record's ok by scope, RenovateNoRecentRun counts only complete full runs.
var logqlOracles = map[string]func(rec map[string]string) (kept bool, value string){
	"RenovateRunFailed": func(rec map[string]string) (bool, string) { return true, rec["ok"] },
	"RenovateNoRecentRun": func(rec map[string]string) (bool, string) {
		return rec["scope"] == "full" && rec["result"] == resultComplete, ""
	},
}

// TestLogQLRulesKeepTheRecordsTheyCount evaluates each rule's pipeline over
// the lines the daemon writes on every completed-run path: it keeps exactly
// the records its oracle names, unwraps their value, carries every label it
// groups by, and keeps no other line. Serial: swaps slog.Default.
func TestLogQLRulesKeepTheRecordsTheyCount(t *testing.T) {
	records, others := completedLines(t)
	for name, oracle := range logqlOracles {
		t.Run(name, func(t *testing.T) {
			r := logqlRuleNamed(t, name)
			for _, line := range records {
				rec := parseLogfmt(t, line)
				got := r.eval(t, line)
				wantKept, wantValue := oracle(rec)
				if got.kept != wantKept {
					t.Errorf("keeps the %s %s record = %v, want %v: %s", rec["scope"], rec["result"], got.kept, wantKept, line)
					continue
				}
				if got.value != wantValue {
					t.Errorf("unwraps %q from the %s %s record, want %q", got.value, rec["scope"], rec["result"], wantValue)
				}
				for l := range strings.SplitSeq(r.groupBy, ",") {
					if l = strings.TrimSpace(l); l != "" && got.kept && got.labels[l] == "" {
						t.Errorf("groups by %s, which the kept %s record does not carry", l, rec["result"])
					}
				}
			}
			for _, line := range others {
				if r.eval(t, line).kept {
					t.Errorf("keeps a line that is no run finished record: %s", line)
				}
			}
		})
	}
}

// TestLogQLRulesSelectOneOfTheTwoRecords binds every rule's line filter to a
// message the daemon writes: the run finished record for a rule with an
// oracle, the daemon stopped line for the rest.
func TestLogQLRulesSelectOneOfTheTwoRecords(t *testing.T) {
	for _, r := range readLogQLRules(t) {
		t.Run(r.name, func(t *testing.T) {
			want := `msg="` + daemonStoppedMsg + `"`
			if _, ok := logqlOracles[r.name]; ok {
				want = `msg="` + runFinishedMsg + `"`
			}
			lf := r.stagesOf(stageLineFilter)
			if len(lf) != 1 || r.stages[0].kind != stageLineFilter || lf[0].key != want {
				t.Errorf("line filters %v, want one leading filter on %s", lf, want)
			}
		})
	}
}

// TestLogQLRulesAvoidTheShapeLokiMisreads refuses, in every shipped rule, the
// pipeline shape misreadShape names; the released heartbeat expression is the
// control that it refuses.
func TestLogQLRulesAvoidTheShapeLokiMisreads(t *testing.T) {
	for _, r := range readLogQLRules(t) {
		t.Run(r.name, func(t *testing.T) {
			if err := misreadShape(r); err != nil {
				t.Errorf("%v: %s", err, r.expr)
			}
		})
	}
	t.Run("a field list ahead of a label filter is refused", func(t *testing.T) {
		r := parseLogQLRule(t, "control", "absent_over_time({container=\"renovate\"} |= `msg=\"run finished\"` | logfmt scope, result | scope=\"full\" | result=\"complete\" [14h])")
		if err := misreadShape(r); !errors.Is(err, errMisreadShape) {
			t.Errorf("misreadShape = %v, want errMisreadShape", err)
		}
	})
}

type timedRecord struct {
	fields map[string]string
	at     time.Duration
}

// lastOK evaluates RenovateRunFailed's range aggregation at `at`: the latest
// record of each scope inside the window, keyed by scope.
func lastOK(t *testing.T, r logqlRule, records []timedRecord, at time.Duration) map[string]float64 {
	t.Helper()
	field := r.unwrapField()
	last := map[string]float64{}
	for _, rec := range records {
		if rec.at > at || at-rec.at >= r.window {
			continue
		}
		v, err := strconv.ParseFloat(rec.fields[field], 64)
		if err != nil {
			t.Fatalf("%s: %s = %q: %v", r.name, field, rec.fields[field], err)
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
	if r.unwrapField() != "ok" || r.threshold != 0 || !strings.Contains(r.expr, "== 0") {
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

// TestLogQLNoRecentRunIsAnAbsenceRule pins the heartbeat's aggregation: it
// fires on silence, so a stopped schedule or lost logs raise it too.
func TestLogQLNoRecentRunIsAnAbsenceRule(t *testing.T) {
	r := logqlRuleNamed(t, "RenovateNoRecentRun")
	if !strings.HasPrefix(r.expr, "absent_over_time(") {
		t.Errorf("%s is not an absent_over_time rule: %s", r.name, r.expr)
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
