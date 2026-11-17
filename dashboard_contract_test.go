package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	dashboardFile = "grafana-dashboard.json"
	promqlRules   = "alerts/promql.yaml"
)

type dashQuery struct {
	Spec struct {
		Query struct {
			Spec struct {
				Expr         string `json:"expr"`
				Query        string `json:"query"`
				LegendFormat string `json:"legendFormat"`
			} `json:"spec"`
		} `json:"query"`
	} `json:"spec"`
}

type valueMapping struct {
	Options map[string]json.RawMessage `json:"options"`
}

type dashPanel struct {
	Spec struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Data        struct {
			Spec struct {
				Queries      []dashQuery `json:"queries"`
				QueryOptions struct {
					MaxDataPoints int `json:"maxDataPoints"`
				} `json:"queryOptions"`
			} `json:"spec"`
		} `json:"data"`
		VizConfig struct {
			Group string `json:"group"`
			Spec  struct {
				FieldConfig struct {
					Defaults struct {
						NoValue  string         `json:"noValue"`
						Mappings []valueMapping `json:"mappings"`
					} `json:"defaults"`
				} `json:"fieldConfig"`
			} `json:"spec"`
		} `json:"vizConfig"`
	} `json:"spec"`
}

type conditionRule struct {
	Kind string `json:"kind"`
	Spec struct {
		Value    any    `json:"value"`
		Variable string `json:"variable"`
		Operator string `json:"operator"`
	} `json:"spec"`
}

type conditionGroup struct {
	Spec struct {
		Visibility string          `json:"visibility"`
		Condition  string          `json:"condition"`
		Items      []conditionRule `json:"items"`
	} `json:"spec"`
}

type gridItem struct {
	Spec struct {
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Element              struct {
			Name string `json:"name"`
		} `json:"element"`
	} `json:"spec"`
}

type layoutNode struct {
	Spec struct {
		Layout               *layoutNode     `json:"layout"`
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Title                string          `json:"title"`
		Rows                 []layoutNode    `json:"rows"`
		Items                []gridItem      `json:"items"`
	} `json:"spec"`
}

type dashVariable struct {
	Kind string `json:"kind"`
	Spec struct {
		Current struct {
			Text  any `json:"text"`
			Value any `json:"value"`
		} `json:"current"`
		Name       string          `json:"name"`
		Regex      string          `json:"regex"`
		Refresh    string          `json:"refresh"`
		Sort       string          `json:"sort"`
		Query      json.RawMessage `json:"query"`
		Multi      bool            `json:"multi"`
		IncludeAll bool            `json:"includeAll"`
	} `json:"spec"`
}

type dashboard struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Elements    map[string]dashPanel `json:"elements"`
		Title       string               `json:"title"`
		Layout      layoutNode           `json:"layout"`
		Variables   []dashVariable       `json:"variables"`
		Annotations []dashAnnotation     `json:"annotations"`
		Preload     bool                 `json:"preload"`
	} `json:"spec"`
}

type dashAnnotation struct {
	Spec struct {
		Name  string `json:"name"`
		Query struct {
			Group string `json:"group"`
			Spec  struct {
				Expr string `json:"expr"`
			} `json:"spec"`
		} `json:"query"`
		BuiltIn bool `json:"builtIn"`
	} `json:"spec"`
}

func loadDashboard(t *testing.T) dashboard {
	t.Helper()
	raw, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardFile, err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decoding %s: %v", dashboardFile, err)
	}
	if len(d.Spec.Elements) == 0 {
		t.Fatalf("%s holds no panel", dashboardFile)
	}
	return d
}

func panelExprs(p *dashPanel) []string {
	var out []string
	for _, q := range p.Spec.Data.Spec.Queries {
		out = append(out, q.Spec.Query.Spec.Expr)
	}
	return out
}

func variableQueries(d *dashboard) map[string]string {
	out := map[string]string{}
	for _, v := range d.Spec.Variables {
		if v.Kind != "QueryVariable" {
			continue
		}
		var q dashQuery
		if err := json.Unmarshal(v.Spec.Query, &q.Spec.Query); err == nil {
			out[v.Spec.Name] = q.Spec.Query.Spec.Query
		}
	}
	return out
}

// writtenQueries returns every query variable's, panel's and Prometheus
// annotation's query as written, keyed by where it sits. The shared query
// check reads a non-built-in annotation of the prometheus group as PromQL.
func writtenQueries(d *dashboard) map[string]string {
	queries := map[string]string{}
	for name, q := range variableQueries(d) {
		queries["variable "+name] = q
	}
	for name, p := range d.Spec.Elements {
		for i, e := range panelExprs(&p) {
			queries[fmt.Sprintf("%s %s query %d", name, p.Spec.Title, i)] = e
		}
	}
	for _, a := range d.Spec.Annotations {
		if !a.Spec.BuiltIn && a.Spec.Query.Group == "prometheus" {
			queries["annotation "+a.Spec.Name] = a.Spec.Query.Spec.Expr
		}
	}
	return queries
}

// dashboardQueries is writtenQueries with # comments removed, so no check
// reads a comment as a query.
func dashboardQueries(d *dashboard) map[string]string {
	queries := writtenQueries(d)
	for where, q := range queries {
		queries[where] = withoutComments(q)
	}
	return queries
}

func dashboardRow(t *testing.T, d *dashboard, title string) *layoutNode {
	t.Helper()
	for i := range d.Spec.Layout.Spec.Rows {
		if r := &d.Spec.Layout.Spec.Rows[i]; r.Spec.Title == title {
			return r
		}
	}
	t.Fatalf("%s has no row titled %q", dashboardFile, title)
	return nil
}

var (
	quotedRe      = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	functionRe    = regexp.MustCompile(`\b[a-z_]+\s*\(`)
	labelValuesRe = regexp.MustCompile(`label_values\(([^,]+),\s*\w+\)`)
	variableRe    = regexp.MustCompile(`\$\{?\w+(?::\w+)?\}?`)
	alertsRe      = regexp.MustCompile(`ALERTS\{alertname="(\w+)"`)
	legendLabelRe = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
	// bracedVariableRe matches ${name}, whose braces are no selector.
	bracedVariableRe = regexp.MustCompile(`\$\{\w+(?::\w+)?\}`)
	// absentCallRe matches a call of absent() or absent_over_time(); PromQL
	// lets whitespace stand before a call's parenthesis.
	absentCallRe = regexp.MustCompile(`(?:^|[^\w:])absent(?:_over_time)?\s*\(`)
	// grafanaVariableRe is grafana v13.2.3's variableRegex
	// (public/app/features/variables/utils.ts): every reference it substitutes.
	grafanaVariableRe  = regexp.MustCompile(`\$(\w+)|\[\[(\w+?)(?::(\w+))?\]\]|\$\{(\w+)(?:\.([^:^}]+))?(?::([^}]+))?\}`)
	readableVariableRe = regexp.MustCompile(`^(?:\$\w+|\$\{\w+(?::\w+)?\})$`)
)

// skipString returns the index just past the string literal opening at s[i].
func skipString(s string, i int) int {
	q := s[i]
	for i++; i < len(s); i++ {
		switch {
		case s[i] == '\\' && q != '`':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return len(s)
}

// withoutComments drops each # comment from expr, up to its newline. A # inside
// a string literal opens no comment, and a quote inside a comment opens no
// string.
func withoutComments(expr string) string {
	var b strings.Builder
	for i := 0; i < len(expr); {
		switch expr[i] {
		case '#':
			if j := strings.IndexByte(expr[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(expr)
			}
		case '"', '\'', '`':
			j := skipString(expr, i)
			b.WriteString(expr[i:j])
			i = j
		default:
			b.WriteByte(expr[i])
			i++
		}
	}
	return b.String()
}

// promqlCode is expr with its string literals and # comments blanked, so a
// function name inside one is not read as a call.
func promqlCode(expr string) string {
	b := []byte(expr)
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '"', '\'', '`':
			end := skipString(expr, i)
			for j := i; j < end; j++ {
				b[j] = ' '
			}
			i = end - 1
		case '#':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

type selectorBlock struct {
	name, text string
}

// selectors returns each {...} block of expr with the series name written
// before it, empty when none, a keyword (x or {...}) or a variable ($v{...})
// is. Quoted strings are skipped, so a brace inside a label value ends no
// block.
func selectors(expr string) []selectorBlock {
	var out []selectorBlock
	start := -1
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; {
		case c == '"' || c == '\'' || c == '`':
			i = skipString(expr, i) - 1
		case c == '{':
			start = i
		case c == '}' && start >= 0:
			before := strings.TrimRight(expr[:start], " \t\r\n")
			name := before[len(strings.TrimRightFunc(before, func(r rune) bool {
				return r == '_' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
			})):]
			if slices.Contains(promqlWords, name) || strings.HasSuffix(before[:len(before)-len(name)], "$") {
				name = ""
			}
			out = append(out, selectorBlock{name: name, text: name + expr[start:i+1]})
			start = -1
		}
	}
	return out
}

// userLabels are labels the user's own scrape or collector names, so a
// legend showing one prints the same value on every series of one instance.
var userLabels = []string{"job", "hostname", "container"}

// dashboardSeries returns the series names a panel or variable query reads;
// label_values names a label after its series, which is no series.
func dashboardSeries(expr string) []string {
	expr = labelValuesRe.ReplaceAllString(expr, "$1")
	expr = labelMatcherRe.ReplaceAllString(expr, " ")
	expr = quotedRe.ReplaceAllString(expr, " ")
	expr = variableRe.ReplaceAllString(expr, " ")
	expr = groupingRe.ReplaceAllString(expr, " ")
	expr = functionRe.ReplaceAllString(expr, "(")
	return seriesNames(expr)
}

// TestDashboardIsTheShippedV2File fails when the file stops being the v2
// resource Grafana 13.2 imports under its stable name and title.
func TestDashboardIsTheShippedV2File(t *testing.T) {
	d := loadDashboard(t)
	if d.APIVersion != "dashboard.grafana.app/v2" || d.Kind != "Dashboard" {
		t.Errorf("apiVersion %q kind %q, want dashboard.grafana.app/v2 Dashboard", d.APIVersion, d.Kind)
	}
	if d.Metadata.Name != "docker-renovate-scheduler" {
		t.Errorf("metadata.name = %q, want docker-renovate-scheduler, which an import keys on", d.Metadata.Name)
	}
	if d.Spec.Title != "Renovate Scheduler" {
		t.Errorf("title = %q, want Renovate Scheduler", d.Spec.Title)
	}
	if !d.Spec.Preload {
		t.Error("spec.preload = false, want true so a hidden tile still queries")
	}
}

// TestDashboardReadsOnlyExportedSeries fails when a panel or a variable reads
// a series, or matches a label, the registry runDaemon serves does not
// export, so a renamed series fails the build instead of blanking a panel.
// It also fails a selector with no series name before its braces, or with a
// name in quotes, which this test and the ALERTS pin cannot read. ALERTS is
// the ruler's series.
func TestDashboardReadsOnlyExportedSeries(t *testing.T) {
	d := loadDashboard(t)
	exported := exportedFamilies(t)
	for where, expr := range dashboardQueries(&d) {
		for _, sel := range selectors(bracedVariableRe.ReplaceAllString(expr, "$$v")) {
			if sel.name == "" {
				t.Errorf("%s has a selector with no series name before its braces, which this check cannot read: %s", where, sel.text)
			}
			if quotesAName(sel.text) {
				t.Errorf("%s names a label or series in quotes, which this check cannot read: %s", where, sel.text)
			}
		}
		names := dashboardSeries(expr)
		if len(names) == 0 {
			t.Errorf("%s reads no series: %s", where, expr)
		}
		for _, name := range names {
			if _, ok := exported[name]; !ok && name != "ALERTS" {
				t.Errorf("%s reads %s, which the registry runDaemon serves does not export", where, name)
			}
		}
		for _, sel := range selectorRe.FindAllStringSubmatch(expr, -1) {
			labels, ok := exported[sel[1]]
			if !ok {
				continue
			}
			for _, m := range matcherNameRe.FindAllStringSubmatch(sel[2], -1) {
				if !slices.Contains(labels, m[1]) && m[1] != "instance" {
					t.Errorf("%s matches label %s on %s, which carries only %v", where, m[1], sel[1], labels)
				}
			}
		}
	}
}

// TestDashboardVariableReferencesAreReadable fails a query holding a Grafana
// variable reference other than $name, ${name} or ${name:format}. Grafana
// takes any character but } in a format, and a quote there would move what
// this file's checks read as a string or a comment.
func TestDashboardVariableReferencesAreReadable(t *testing.T) {
	d := loadDashboard(t)
	for where, q := range writtenQueries(&d) {
		for _, ref := range grafanaVariableRe.FindAllString(q, -1) {
			if !readableVariableRe.MatchString(ref) {
				t.Errorf("%s uses %s, a variable reference these checks cannot read; write $name, ${name} or ${name:format}", where, ref)
			}
		}
	}
}

// TestDashboardLegendsShowWhatVaries fails when a legend shows a label that is
// the same on every series of one instance, which labels nothing.
func TestDashboardLegendsShowWhatVaries(t *testing.T) {
	d := loadDashboard(t)
	for name, p := range d.Spec.Elements {
		for _, q := range p.Spec.Data.Spec.Queries {
			for _, m := range legendLabelRe.FindAllStringSubmatch(q.Spec.Query.Spec.LegendFormat, -1) {
				if m[1] == "instance" || slices.Contains(userLabels, m[1]) {
					t.Errorf("%s %q legend shows %s, which does not vary inside one instance", name, p.Spec.Title, m[1])
				}
			}
		}
	}
}

// TestDashboardInstanceVariablePicksOneScheduler fails when the instance
// picker needs setup or hides a scheduler: it lists every scheduler from
// build_info with no regex filter, refreshes with the time range, picks the
// first on load and shows one at a time.
func TestDashboardInstanceVariablePicksOneScheduler(t *testing.T) {
	d := loadDashboard(t)
	var v *dashVariable
	for i := range d.Spec.Variables {
		if d.Spec.Variables[i].Spec.Name == "instance" {
			v = &d.Spec.Variables[i]
		}
	}
	if v == nil {
		t.Fatal("no instance variable")
	}
	if got, want := variableQueries(&d)["instance"], "label_values("+metricsPrefix+"_build_info, instance)"; got != want {
		t.Errorf("instance query = %q, want %q", got, want)
	}
	if v.Kind != "QueryVariable" || v.Spec.Multi || v.Spec.IncludeAll {
		t.Errorf("instance is %s multi=%t includeAll=%t, want a single-select QueryVariable", v.Kind, v.Spec.Multi, v.Spec.IncludeAll)
	}
	if v.Spec.Sort != "alphabeticalAsc" || v.Spec.Refresh != "onTimeRangeChanged" {
		t.Errorf("instance sort %q refresh %q, want alphabeticalAsc onTimeRangeChanged", v.Spec.Sort, v.Spec.Refresh)
	}
	if v.Spec.Regex != "" {
		t.Errorf("instance regex = %q, want empty: a regex hides schedulers its query lists", v.Spec.Regex)
	}
	if v.Spec.Current.Value != "" || v.Spec.Current.Text != "" {
		t.Errorf("instance current = %v, want empty so Grafana picks the first instance", v.Spec.Current.Value)
	}
}

// attentionReads is every ALERTS selector a query may read the rule name's
// firing alerts with, for the rule expression expr, in its tile's order.
// absent() and absent_over_time() label their alert with only the selector's
// equality matchers, and a shipped rule's selector names no instance, so only
// such a rule's alert is also read with instance="":
// https://prometheus.io/docs/prometheus/latest/querying/functions/#absent
func attentionReads(name, expr string) []string {
	read := `ALERTS{alertname="` + name + `", alertstate="firing", instance=`
	if absentCallRe.MatchString(promqlCode(expr)) {
		return []string{read + `"$instance"}`, read + `""}`}
	}
	return []string{read + `"$instance"}`}
}

// attentionTile is the query of the Needs attention tile for the rule name
// with expression expr, which answers while any of its reads does.
func attentionTile(name, expr string) string {
	reads := attentionReads(name, expr)
	if len(reads) == 2 {
		return `0 * max((` + reads[0] + ` or ` + reads[1] + `))`
	}
	return `0 * max(` + reads[0] + `)`
}

// TestDashboardAttentionTilesMirrorEveryShippedRule fails when a rule in
// alerts/promql.yaml has no Needs attention tile, a tile names a rule the file
// does not hold or does not name it in its description, a tile's query is not
// exactly attentionTile's for its rule, the row's variable does not count
// exactly those tiles in layout order, a tile is not hidden until its query
// answers, any query reads an ALERTS selector that is not one of
// attentionReads' for a shipped rule, or a query reads a rule with
// instance="" but not with instance="$instance".
func TestDashboardAttentionTilesMirrorEveryShippedRule(t *testing.T) {
	d := loadDashboard(t)
	rules := ruleExprs(t, promqlRules)
	for where, expr := range dashboardQueries(&d) {
		sels := selectors(bracedVariableRe.ReplaceAllString(expr, "$$v"))
		var texts []string
		for _, sel := range sels {
			texts = append(texts, sel.text)
		}
		for _, sel := range sels {
			if sel.name != "ALERTS" {
				continue
			}
			name := ""
			if m := alertsRe.FindStringSubmatch(sel.text); m != nil {
				name = m[1]
			}
			rule, ok := rules[name]
			switch {
			case name == "":
				t.Errorf("%s reads %s, which does not start with alertname=\"<rule>\": every ALERTS read is one of attentionReads' texts, alertname first", where, sel.text)
				continue
			case !ok:
				t.Errorf("%s reads %s, whose alertname names no rule in %s", where, sel.text, promqlRules)
				continue
			}
			reads := attentionReads(name, rule)
			switch {
			case !slices.Contains(reads, sel.text):
				t.Errorf("%s reads %s; a read of %s must be exactly one of these, so it shows only the picked scheduler's firing alerts:\n%s", where, sel.text, name, strings.Join(reads, "\n"))
			case strings.HasSuffix(sel.text, `instance=""}`) && !slices.Contains(texts, reads[0]):
				t.Errorf("%s reads %s without %s in the same query: a read of only instance=\"\" misses the rule's alerts for the picked scheduler", where, sel.text, reads[0])
			}
		}
	}
	attention := dashboardRow(t, &d, "Needs attention")
	if d.Spec.Layout.Spec.Rows[0].Spec.Title != "Needs attention" {
		t.Error("Needs attention is not the first row")
	}
	gate := attention.Spec.ConditionalRendering
	if gate == nil || gate.Spec.Visibility != "show" || len(gate.Spec.Items) != 1 ||
		gate.Spec.Items[0].Kind != "ConditionalRenderingVariable" ||
		gate.Spec.Items[0].Spec.Operator != "matches" || gate.Spec.Items[0].Spec.Value != "^[1-9]" {
		t.Fatal("Needs attention is not shown by a variable matching ^[1-9]")
	}
	var mirrored, counts []string
	for _, it := range attention.Spec.Layout.Spec.Items {
		p := d.Spec.Elements[it.Spec.Element.Name]
		cr := it.Spec.ConditionalRendering
		var conds []string
		if cr != nil {
			for _, c := range cr.Spec.Items {
				conds = append(conds, fmt.Sprintf("%s=%v", c.Kind, c.Spec.Value))
			}
		}
		slices.Sort(conds)
		if cr == nil || cr.Spec.Visibility != "show" || cr.Spec.Condition != "or" ||
			!slices.Equal(conds, []string{"ConditionalRenderingData=true", "ConditionalRenderingTimeRangeSize=0s"}) ||
			p.Spec.Data.Spec.QueryOptions.MaxDataPoints <= 0 {
			t.Errorf("%q does not show exactly while its query answers: want a shown group or'ing a has-data rule with a 0s range rule, and a maxDataPoints pin", p.Spec.Title)
		}
		exprs := panelExprs(&p)
		if len(exprs) != 1 {
			t.Errorf("%q holds %d queries, want 1", p.Spec.Title, len(exprs))
			continue
		}
		counts = append(counts, fmt.Sprintf(`label_replace(count(%s), "tile", "%s", "", "")`, exprs[0], strings.TrimPrefix(it.Spec.Element.Name, "panel-")))
		m := alertsRe.FindStringSubmatch(exprs[0])
		if m == nil {
			t.Errorf("%q reads no ALERTS series by alertname: %s", p.Spec.Title, exprs[0])
			continue
		}
		name := m[1]
		mirrored = append(mirrored, name)
		if !strings.Contains(p.Spec.Description, name) {
			t.Errorf("%q reads %s, which its description does not name", p.Spec.Title, name)
		}
		expr, ok := rules[name]
		if !ok {
			t.Errorf("a tile mirrors %s, which %s does not hold", name, promqlRules)
			continue
		}
		if want := attentionTile(name, expr); exprs[0] != want {
			t.Errorf("%q must read exactly\n%s\nso it shows only the picked scheduler's firing %s alerts; it reads\n%s",
				p.Spec.Title, want, name, exprs[0])
		}
	}
	if got, want := variableQueries(&d)[gate.Spec.Items[0].Spec.Variable], "query_result(count("+strings.Join(counts, " or ")+") or vector(0))"; got != want {
		t.Errorf("the row's variable must count exactly its tiles in layout order:\n%s\nit reads\n%s", want, got)
	}
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		if !slices.Contains(mirrored, name) {
			t.Errorf("%s has no Needs attention tile reading its firing alerts", name)
		}
	}
}

func TestDashboardPanelsExplainThemselves(t *testing.T) {
	d := loadDashboard(t)
	for name, p := range d.Spec.Elements {
		if strings.TrimSpace(p.Spec.Description) == "" {
			t.Errorf("%s %q has no description", name, p.Spec.Title)
		}
		switch p.Spec.VizConfig.Group {
		case "stat", "gauge", "bargauge", "table":
			if strings.TrimSpace(p.Spec.VizConfig.Spec.FieldConfig.Defaults.NoValue) == "" {
				t.Errorf("%s %q has no no-value text", name, p.Spec.Title)
			}
		}
	}
}

var idleArmRe = regexp.MustCompile(` or on \(instance\) \((-?\d+) \* max by \(instance\) \(` +
	metricsPrefix + `_build_info\{instance="\$instance"\}\)\)$`)

// TestDashboardRunTilesTellIdleFromUnscraped fails when a Runs tile can show
// its idle text for a scheduler Prometheus does not scrape. Each tile falls
// back to a mapped sentinel only while build_info answers, so an empty answer
// means not scraped and reads so.
func TestDashboardRunTilesTellIdleFromUnscraped(t *testing.T) {
	d := loadDashboard(t)
	tiles := 0
	for _, it := range dashboardRow(t, &d, "Runs").Spec.Layout.Spec.Items {
		p := d.Spec.Elements[it.Spec.Element.Name]
		if p.Spec.VizConfig.Group != "stat" {
			continue
		}
		tiles++
		defaults := p.Spec.VizConfig.Spec.FieldConfig.Defaults
		if defaults.NoValue != "Not scraped" {
			t.Errorf("%q reads %q with no answer, want Not scraped", p.Spec.Title, defaults.NoValue)
		}
		for _, e := range panelExprs(&p) {
			m := idleArmRe.FindStringSubmatch(e)
			if m == nil {
				t.Errorf("%q has no idle arm on build_info: %s", p.Spec.Title, e)
				continue
			}
			mapped := slices.ContainsFunc(defaults.Mappings, func(mp valueMapping) bool {
				_, ok := mp.Options[m[1]]
				return ok
			})
			if !mapped {
				t.Errorf("%q idle value %s has no value mapping", p.Spec.Title, m[1])
			}
		}
	}
	if tiles == 0 {
		t.Fatal("the Runs row holds no stat tile")
	}
}

// TestDashboardRunsByResultIsTheTestedQuery fails when the Runs by result
// query differs from the one alerts/promql_test.yaml evaluates across
// restarts, so a change to its slot arithmetic runs through promtool in CI.
func TestDashboardRunsByResultIsTheTestedQuery(t *testing.T) {
	d := loadDashboard(t)
	var exprs []string
	for _, p := range d.Spec.Elements {
		if p.Spec.Title == "Runs by result" {
			exprs = append(exprs, panelExprs(&p)...)
		}
	}
	if len(exprs) != 1 {
		t.Fatalf("found %d Runs by result queries, want 1", len(exprs))
	}
	want := strings.NewReplacer(`$instance`, "renovate:9470", `${__interval_ms}`, "300000",
		`$__interval`, "5m").Replace(exprs[0])
	raw, err := os.ReadFile("alerts/promql_test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)expr: &runs_by_result '([^']*)'$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("alerts/promql_test.yaml has no expr anchored as runs_by_result")
	}
	if string(m[1]) != want {
		t.Errorf("alerts/promql_test.yaml tests\n%s\nthe dashboard reads, with its variables filled in,\n%s", m[1], want)
	}
}
