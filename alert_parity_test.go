package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/scheduler"
)

// promqlWords are the PromQL keywords and functions the shipped rules may use;
// every other identifier in an expression is a series name.
var promqlWords = []string{
	"and", "or", "unless", "on", "ignoring", "group_left", "group_right", "by", "without", "bool",
	"sum", "min", "max", "avg", "count", "absent", "time", "rate", "increase", "changes", "resets",
	"min_over_time", "max_over_time", "avg_over_time", "sum_over_time", "count_over_time",
	"last_over_time", "present_over_time", "absent_over_time",
}

// scrapeLabels are set by the scrape on every series, so a selector may match
// them on any family.
var scrapeLabels = []string{"job", "instance"}

// seriesOutsideTheApp are names a rule may read that no app registry exports:
// `up` is written by the scrape itself.
var seriesOutsideTheApp = []string{"up"}

var (
	selectorRe     = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}`)
	matcherNameRe  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)`)
	labelMatcherRe = regexp.MustCompile(`\{[^}]*\}`)
	groupingRe     = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	promRangeRe    = regexp.MustCompile(`\[[^\]]*\]`)
	identRe        = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
	ageSecondsRe   = regexp.MustCompile(`>\s*(\d+)`)
)

// ruleExprs returns each alert of a shipped rule file with its expression.
func ruleExprs(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	rules := make(map[string]string)
	for _, block := range strings.Split(string(raw), "- alert: ")[1:] {
		name, rest, _ := strings.Cut(block, "\n")
		_, expr, found := strings.Cut(rest, "expr: ")
		if !found {
			t.Fatalf("%s: %s has no expr", path, name)
		}
		expr, _, _ = strings.Cut(expr, "for:")
		rules[strings.TrimSpace(name)] = strings.TrimPrefix(strings.TrimSpace(expr), "|")
	}
	return rules
}

// seriesNames returns the series an expression reads.
func seriesNames(expr string) []string {
	expr = labelMatcherRe.ReplaceAllString(expr, " ")
	expr = groupingRe.ReplaceAllString(expr, " ")
	expr = promRangeRe.ReplaceAllString(expr, " ")
	var names []string
	for _, word := range identRe.FindAllString(expr, -1) {
		if !slices.Contains(promqlWords, word) && !slices.Contains(names, word) {
			names = append(names, word)
		}
	}
	return names
}

// exportedFamilies builds the registry runDaemon serves, and runs one complete
// run of each scope through it, because a labelled family writes nothing
// before its first sample. It returns each family's label names.
func exportedFamilies(t *testing.T) map[string][]string {
	t.Helper()
	dir := t.TempDir()
	reg, m := newMetricsRegistry("built-in", "token", scheduler.NewStamp(filepath.Join(dir, "completed")))
	now := time.Now()
	for _, scope := range runScopes {
		m.observe(scope, runVerdict{result: resultComplete, ok: true}, time.Second, now, time.Time{})
	}
	return familyLabels(t, reg)
}

func familyLabels(t *testing.T, reg *measure.Registry) map[string][]string {
	t.Helper()
	families, err := reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather = %v", err)
	}
	labels := make(map[string][]string, len(families))
	for _, f := range families {
		if len(f.Samples) == 0 {
			t.Errorf("%s has no sample after a run of each scope, so a rule over it sees nothing", f.Name)
		}
		var names []string
		for _, s := range f.Samples {
			for _, l := range s.Labels {
				if !slices.Contains(names, l.Name) {
					names = append(names, l.Name)
				}
			}
		}
		labels[f.Name] = names
	}
	return labels
}

// TestPromQLRulesReadOnlyExportedSeries fails when a shipped PromQL rule reads
// a series, or matches a label, that the registry runDaemon serves does not
// export, so a renamed or dropped series fails the build instead of silencing
// an alert.
func TestPromQLRulesReadOnlyExportedSeries(t *testing.T) {
	exported := exportedFamilies(t)
	for alert, expr := range ruleExprs(t, "alerts/promql.yaml") {
		names := seriesNames(expr)
		if len(names) == 0 {
			t.Errorf("%s reads no series: %s", alert, expr)
		}
		for _, name := range names {
			if _, ok := exported[name]; !ok && !slices.Contains(seriesOutsideTheApp, name) {
				t.Errorf("%s reads %s, which the registry runDaemon serves does not export", alert, name)
			}
		}
		for _, sel := range selectorRe.FindAllStringSubmatch(expr, -1) {
			labels, ok := exported[sel[1]]
			if !ok {
				continue
			}
			for _, m := range matcherNameRe.FindAllStringSubmatch(sel[2], -1) {
				if !slices.Contains(labels, m[1]) && !slices.Contains(scrapeLabels, m[1]) {
					t.Errorf("%s matches label %s on %s, which carries only %v", alert, m[1], sel[1], labels)
				}
			}
		}
	}
}

// oneSided names the alerts that exist in one rule file only, with the alert
// that covers the same problem in the other file.
var oneSided = map[string]string{
	"RenovateDaemonStopped": "RenovateDown",
	"RenovateDown":          "RenovateDaemonStopped",
}

// releasedAlerts are the alert names an earlier release shipped, each with the
// name that carries it now, so dropping or renaming one is a decision in the
// diff.
var releasedAlerts = map[string]string{
	"RenovateRunFailed":   "RenovateRunFailed",
	"RenovateNoRecentRun": "RenovateNoRecentRun",
}

// TestAlertFilesHoldTheSameAlerts keeps the two alternative files in parity:
// an alert in one has a partner of the same name in the other, except the
// one-sided alerts named with their cover, and every released alert name
// still maps to an alert both files define.
func TestAlertFilesHoldTheSameAlerts(t *testing.T) {
	promql := ruleExprs(t, "alerts/promql.yaml")
	logql := ruleExprs(t, "alerts/logql.yaml")
	for _, pair := range []struct {
		name        string
		rules, peer map[string]string
	}{
		{name: "alerts/promql.yaml", rules: promql, peer: logql},
		{name: "alerts/logql.yaml", rules: logql, peer: promql},
	} {
		for alert := range pair.rules {
			if _, ok := pair.peer[alert]; ok {
				continue
			}
			cover, named := oneSided[alert]
			if !named {
				t.Errorf("%s holds %s, which the other file lacks and oneSided does not name", pair.name, alert)
				continue
			}
			if _, ok := pair.peer[cover]; !ok {
				t.Errorf("%s exists only in %s, and its cover %s is missing from the other file", alert, pair.name, cover)
			}
		}
	}
	for released, now := range releasedAlerts {
		_, inProm := promql[now]
		_, inLog := logql[now]
		if !inProm || !inLog {
			t.Errorf("released alert %s maps to %s, which is not in both files", released, now)
		}
	}
}

// TestHeadersNameTheOneSidedAlerts keeps each one-sided alert and its cover in
// both file headers, where a user deciding which file to load reads them.
func TestHeadersNameTheOneSidedAlerts(t *testing.T) {
	for _, path := range []string{"alerts/promql.yaml", "alerts/logql.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		header, _, _ := strings.Cut(string(raw), "groups:")
		for alert, cover := range oneSided {
			if !strings.Contains(header, alert) || !strings.Contains(header, cover) {
				t.Errorf("%s header does not name %s and its cover %s", path, alert, cover)
			}
		}
	}
}

// TestPromQLNoRecentRunWindowIsTwiceTheDefaultRunCycle keeps the metric rule
// at the log rule's window: twice the sum of RUN_INTERVAL and RUN_TIMEOUT.
func TestPromQLNoRecentRunWindowIsTwiceTheDefaultRunCycle(t *testing.T) {
	want := 2 * (defaultInterval + defaultRunTimeout)
	expr, ok := ruleExprs(t, "alerts/promql.yaml")["RenovateNoRecentRun"]
	if !ok {
		t.Fatal("alerts/promql.yaml has no RenovateNoRecentRun rule")
	}
	matches := ageSecondsRe.FindAllStringSubmatch(expr, -1)
	if len(matches) == 0 {
		t.Fatalf("RenovateNoRecentRun compares no age in seconds: %s", expr)
	}
	for _, m := range matches {
		seconds, _ := strconv.Atoi(m[1])
		if got := time.Duration(seconds) * time.Second; got != want {
			t.Errorf("RenovateNoRecentRun waits %s, want %s", got, want)
		}
	}
}
