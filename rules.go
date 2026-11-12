package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"

	"gopkg.in/yaml.v3"
)

// Prometheus rule types

type PrometheusRules struct {
	Groups []RuleGroup `yaml:"groups"`
}

type RuleGroup struct {
	Name     string `yaml:"name"`
	Interval string `yaml:"interval,omitempty"`
	Rules    []Rule `yaml:"rules"`
}

type Rule struct {
	Record      string            `yaml:"record,omitempty"`
	Alert       string            `yaml:"alert,omitempty"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// Recording rule names. Every series also carries `service` and `slo`
// labels, so any number of SLOs can share these names without clashing.
const (
	recBad5m   = "slo:sli_bad:rate5m"
	recTotal5m = "slo:sli_total:rate5m"
)

func recordName(window string) string {
	return "slo:sli_error:ratio_rate" + window
}

// alertWindows must cover every short/long window used in burnRates.
var alertWindows = []string{"5m", "30m", "1h", "6h", "1d", "3d"}

func (s Spec) labels() map[string]string {
	return map[string]string{"service": s.Service, "slo": s.Name}
}

// matcher selects this SLO's recorded series.
func (s Spec) matcher() string {
	return fmt.Sprintf(`{service=%q,slo=%q}`, s.Service, s.Name)
}

// badRate is the per-second rate of bad events over a window.
func (s Spec) badRate(w string) string {
	if s.Type == typeLatency {
		return fmt.Sprintf("sum(rate(%s[%s])) - sum(rate(%s[%s]))", s.TotalSelector, w, s.GoodSelector, w)
	}
	return fmt.Sprintf("sum(rate(%s[%s]))", s.BadSelector, w)
}

func (s Spec) totalRate(w string) string {
	return fmt.Sprintf("sum(rate(%s[%s]))", s.TotalSelector, w)
}

func generateRecordingRules(s Spec) (string, error) {
	rules := []Rule{
		{Record: recBad5m, Expr: s.badRate("5m"), Labels: s.labels()},
		{Record: recTotal5m, Expr: s.totalRate("5m"), Labels: s.labels()},
		{
			Record: recordName("5m"),
			Expr:   fmt.Sprintf("%s%s\n/\n%s%s", recBad5m, s.matcher(), recTotal5m, s.matcher()),
			Labels: s.labels(),
		},
	}
	for _, w := range alertWindows[1:] {
		rules = append(rules, Rule{
			Record: recordName(w),
			Expr:   fmt.Sprintf("(%s)\n/\n%s", s.badRate(w), s.totalRate(w)),
			Labels: s.labels(),
		})
	}
	// The budget window adds up the 5m rates recorded above rather than
	// scanning weeks of raw samples. Summing bad and total separately keeps
	// it weighted by traffic, so quiet hours don't count as much as busy ones.
	rules = append(rules, Rule{
		Record: recordName(s.Window),
		Expr: fmt.Sprintf("sum_over_time(%s%s[%s])\n/\nsum_over_time(%s%s[%s])",
			recBad5m, s.matcher(), s.Window, recTotal5m, s.matcher(), s.Window),
		Labels: s.labels(),
	})
	out := PrometheusRules{Groups: []RuleGroup{{
		Name:     fmt.Sprintf("%s-%s.slo.rules", s.Service, s.Name),
		Interval: "30s",
		Rules:    rules,
	}}}
	b, err := yaml.Marshal(out)
	return string(b), err
}

type burnRate struct {
	name, severity, short, long, forDur string
	burn                                float64
}

var burnRates = []burnRate{
	{"CriticalFast", "critical", "5m", "1h", "2m", 14.4},
	{"CriticalSlow", "critical", "30m", "6h", "15m", 6},
	{"WarningFast", "warning", "1h", "1d", "1h", 3},
	{"WarningSlow", "warning", "6h", "3d", "3h", 1},
}

func generateAlertingRules(s Spec) (string, error) {
	errorRate := 1 - s.SLO/100
	rules := make([]Rule, len(burnRates))
	for i, br := range burnRates {
		threshold := errorRate * br.burn
		labels := s.labels()
		labels["severity"] = br.severity
		rules[i] = Rule{
			Alert: fmt.Sprintf("%s%sErrorBudget%s", camel(s.Service), camel(s.Name), br.name),
			Expr: fmt.Sprintf("%s%s > %.6f\nand\n%s%s > %.6f",
				recordName(br.short), s.matcher(), threshold, recordName(br.long), s.matcher(), threshold),
			For:    br.forDur,
			Labels: labels,
			Annotations: map[string]string{
				"summary": fmt.Sprintf("%s %s SLO is burning error budget too fast (%s)", s.Service, s.Name, br.name),
				"description": fmt.Sprintf(
					"%s %s SLO (%s) is using its error budget at %.1fx the sustainable rate. Bad ratio over %s: {{ $value | humanizePercentage }}.",
					s.Service, s.Name, s.Describe(), br.burn, br.short,
				),
			},
		}
	}
	out := PrometheusRules{Groups: []RuleGroup{{
		Name:  fmt.Sprintf("%s-%s.slo.alerts", s.Service, s.Name),
		Rules: rules,
	}}}
	b, err := yaml.Marshal(out)
	return string(b), err
}

// Dashboard schema version 39 is what Grafana 10.4 writes natively, so it
// imports there with no migration. Newer versions upgrade it on load.
const grafanaSchemaVersion = 39

// dashboardUID derives a stable UID from the service and SLO name, so
// re-importing a regenerated dashboard updates the existing one instead of
// duplicating it. Grafana UIDs are limited to 40 characters.
func dashboardUID(service, name string) string {
	uid := "slo-" + metricPrefix(service) + "-" + name
	if len(uid) <= 40 {
		return uid
	}
	h := fnv.New32a()
	h.Write([]byte(service + "/" + name))
	return fmt.Sprintf("%s-%08x", uid[:31], h.Sum32())
}

// Panels reference the datasource through a dashboard variable, so the
// same JSON works via the import UI, the HTTP API and file provisioning.
var promDatasource = map[string]string{"type": "prometheus", "uid": "${datasource}"}

func promTarget(expr, legend string) []map[string]any {
	return []map[string]any{{
		"refId":        "A",
		"datasource":   promDatasource,
		"expr":         expr,
		"legendFormat": legend,
	}}
}

func generateGrafanaDashboard(s Spec) (string, error) {
	ratioTitle, gaugeTitle := "Error ratio (5m)", fmt.Sprintf("Availability over %s (SLO %g%%)", s.Window, s.SLO)
	if s.Type == typeLatency {
		ratioTitle = fmt.Sprintf("Requests slower than %gms (5m)", s.ThresholdMs)
		gaugeTitle = fmt.Sprintf("Requests under %gms over %s (SLO %g%%)", s.ThresholdMs, s.Window, s.SLO)
	}
	dashboard := map[string]any{
		"uid":           dashboardUID(s.Service, s.Name),
		"title":         fmt.Sprintf("%s %s SLO", s.Service, s.Name),
		"description":   s.Describe(),
		"tags":          []string{"slo", "observability", "ithildin"},
		"schemaVersion": grafanaSchemaVersion,
		"version":       1,
		"editable":      true,
		"timezone":      "browser",
		"refresh":       "1m",
		"time":          map[string]string{"from": "now-24h", "to": "now"},
		"templating": map[string]any{
			"list": []map[string]any{{
				"name":       "datasource",
				"label":      "Data source",
				"type":       "datasource",
				"query":      "prometheus",
				"current":    map[string]any{},
				"options":    []any{},
				"refresh":    1,
				"hide":       0,
				"includeAll": false,
				"multi":      false,
			}},
		},
		"annotations": map[string]any{"list": []any{}},
		"panels": []map[string]any{
			{
				"id":         1,
				"title":      ratioTitle,
				"type":       "timeseries",
				"datasource": promDatasource,
				"gridPos":    map[string]int{"h": 8, "w": 12, "x": 0, "y": 0},
				"targets":    promTarget(recordName("5m")+s.matcher(), "bad ratio"),
				"fieldConfig": map[string]any{
					"defaults":  map[string]any{"unit": "percentunit"},
					"overrides": []any{},
				},
			},
			{
				"id":         2,
				"title":      gaugeTitle,
				"type":       "gauge",
				"datasource": promDatasource,
				"gridPos":    map[string]int{"h": 8, "w": 12, "x": 12, "y": 0},
				"targets":    promTarget("1 - "+recordName(s.Window)+s.matcher(), "good ratio"),
				"fieldConfig": map[string]any{
					"defaults": map[string]any{
						"unit":     "percentunit",
						"decimals": 3,
						"thresholds": map[string]any{
							"mode": "absolute",
							"steps": []map[string]any{
								{"color": "red", "value": nil},
								{"color": "yellow", "value": s.SLO/100 - 0.001},
								{"color": "green", "value": s.SLO / 100},
							},
						},
					},
					"overrides": []any{},
				},
			},
		},
	}
	b, err := json.MarshalIndent(dashboard, "", "  ")
	return string(b), err
}

// Artifacts is everything generated for one SLO.
type Artifacts struct {
	RecordingRules   string `json:"recording_rules"`
	AlertingRules    string `json:"alerting_rules"`
	GrafanaDashboard string `json:"grafana_dashboard"`
}

func generate(s Spec) (Artifacts, error) {
	var a Artifacts
	var err error
	if a.RecordingRules, err = generateRecordingRules(s); err != nil {
		return a, err
	}
	if a.AlertingRules, err = generateAlertingRules(s); err != nil {
		return a, err
	}
	a.GrafanaDashboard, err = generateGrafanaDashboard(s)
	return a, err
}
