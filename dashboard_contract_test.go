package main

import (
	"encoding/json"
	"fmt"
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
						NoValue string `json:"noValue"`
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
		Elements  map[string]dashPanel `json:"elements"`
		Title     string               `json:"title"`
		Layout    layoutNode           `json:"layout"`
		Variables []dashVariable       `json:"variables"`
		Preload   bool                 `json:"preload"`
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

// variableQueries returns each query variable's PromQL by variable name.
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

// allQueries returns every variable and panel query, keyed by where it sits.
func allQueries(d *dashboard) map[string]string {
	queries := variableQueries(d)
	for name, p := range d.Spec.Elements {
		for i, e := range panelExprs(&p) {
			queries[fmt.Sprintf("%s %s query %d", name, p.Spec.Title, i)] = e
		}
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
	quotedRe       = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	functionRe     = regexp.MustCompile(`\b[a-z_]+\s*\(`)
	labelValuesRe  = regexp.MustCompile(`label_values\(([^,]+),\s*\w+\)`)
	variableRe     = regexp.MustCompile(`\$\{?\w+(?::\w+)?\}?`)
	alertsRe       = regexp.MustCompile(`ALERTS\{alertname="([A-Za-z]+)"([^}]*)\}`)
	valueMatcherRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*"((?:[^"\\]|\\.)*)"`)
	legendLabelRe  = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
)

// userLabels are labels the user's own scrape or collector names, so a
// shipped query that matches on one is empty under any other choice.
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
// ALERTS is the ruler's series.
func TestDashboardReadsOnlyExportedSeries(t *testing.T) {
	d := loadDashboard(t)
	exported := exportedFamilies(t)
	for where, expr := range allQueries(&d) {
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

// TestDashboardScopesEveryQueryToTheInstance fails when a query reads a series
// without the picked instance, matches a label the user's own config names,
// or a legend shows a label that is the same on every series of one instance.
func TestDashboardScopesEveryQueryToTheInstance(t *testing.T) {
	d := loadDashboard(t)
	for where, expr := range allQueries(&d) {
		for _, sel := range selectorRe.FindAllStringSubmatch(expr, -1) {
			scoped := false
			for _, m := range valueMatcherRe.FindAllStringSubmatch(sel[2], -1) {
				switch {
				case slices.Contains(userLabels, m[1]):
					t.Errorf("%s matches %s on %s, a label the user's own config names", where, m[1], sel[1])
				case m[1] == "instance":
					scoped = m[2] == "=" && m[3] == "$instance" ||
						m[2] == "=~" && slices.Contains(strings.Split(m[3], "|"), "$instance")
				}
			}
			if !scoped {
				t.Errorf("%s reads %s without instance=\"$instance\"", where, sel[1])
			}
		}
		// The instance variable lists every instance, so it alone reads unscoped.
		if bare := dashboardSeries(selectorRe.ReplaceAllString(expr, " ")); where != "instance" && len(bare) > 0 {
			t.Errorf("%s reads %v without a label matcher", where, bare)
		}
	}
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
// picker needs setup: it lists every scheduler from build_info, picks the
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
	if v.Spec.Current.Value != "" || v.Spec.Current.Text != "" {
		t.Errorf("instance current = %v, want empty so Grafana picks the first instance", v.Spec.Current.Value)
	}
}

// TestDashboardAttentionTilesMirrorEveryShippedRule fails when a rule in
// alerts/promql.yaml has no tile, or a tile names a rule the file does not
// hold. Each tile reads its rule's firing ALERTS series, so it shows exactly
// while the alert fires, and the row shows only while one tile has data.
func TestDashboardAttentionTilesMirrorEveryShippedRule(t *testing.T) {
	d := loadDashboard(t)
	rules := ruleExprs(t, promqlRules)
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
	counted := variableQueries(&d)[gate.Spec.Items[0].Spec.Variable]
	var mirrored []string
	for _, it := range attention.Spec.Layout.Spec.Items {
		p := d.Spec.Elements[it.Spec.Element.Name]
		cr := it.Spec.ConditionalRendering
		var conds []string
		if cr != nil {
			for _, c := range cr.Spec.Items {
				conds = append(conds, c.Kind)
			}
		}
		slices.Sort(conds)
		if cr == nil || cr.Spec.Visibility != "show" || cr.Spec.Condition != "or" ||
			!slices.Equal(conds, []string{"ConditionalRenderingData", "ConditionalRenderingTimeRangeSize"}) ||
			p.Spec.Data.Spec.QueryOptions.MaxDataPoints <= 0 {
			t.Errorf("%q does not show exactly while its query answers: want a shown group or'ing a has-data rule with a 0s range rule, and a maxDataPoints pin", p.Spec.Title)
		}
		for _, e := range panelExprs(&p) {
			if !strings.Contains(counted, "count("+e+")") {
				t.Errorf("%q query is not counted by the row's variable", p.Spec.Title)
			}
			for _, m := range alertsRe.FindAllStringSubmatch(e, -1) {
				if !strings.Contains(m[2], `alertstate="firing"`) {
					t.Errorf("%q reads %s alerts in any state, want only firing ones", p.Spec.Title, m[1])
				}
				if !strings.Contains(p.Spec.Description, m[1]) {
					t.Errorf("%q reads %s, which its description does not name", p.Spec.Title, m[1])
				}
				if !slices.Contains(mirrored, m[1]) {
					mirrored = append(mirrored, m[1])
				}
			}
		}
	}
	for name := range rules {
		if !slices.Contains(mirrored, name) {
			t.Errorf("%s has no attention tile", name)
		}
	}
	for _, name := range mirrored {
		if _, ok := rules[name]; !ok {
			t.Errorf("a tile mirrors %s, which %s does not hold", name, promqlRules)
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
