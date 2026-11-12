package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func avail(service, source string) Input {
	return Input{Service: service, SLO: "99.9", Source: source}
}

func latency(service, source, ms string) Input {
	return Input{Service: service, SLO: "99", Type: "latency", Source: source, ThresholdMs: ms}
}

func mustSpec(t *testing.T, in Input) Spec {
	t.Helper()
	s, err := newSpec(in)
	if err != nil {
		t.Fatalf("newSpec(%+v): %v", in, err)
	}
	return s
}

func parseRules(t *testing.T, y string) PrometheusRules {
	t.Helper()
	var pr PrometheusRules
	if err := yaml.Unmarshal([]byte(y), &pr); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, y)
	}
	return pr
}

func recordingRules(t *testing.T, s Spec) map[string]Rule {
	t.Helper()
	y, err := generateRecordingRules(s)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]Rule{}
	for _, g := range parseRules(t, y).Groups {
		for _, r := range g.Rules {
			rules[r.Record] = r
		}
	}
	return rules
}

// Every window an alert reads must have a recording rule, or the alert
// silently never fires.
func TestAlertWindowsAreRecorded(t *testing.T) {
	for _, in := range []Input{avail("payment-api", ""), latency("payment-api", "", "300")} {
		rules := recordingRules(t, mustSpec(t, in))
		for _, br := range burnRates {
			for _, w := range []string{br.short, br.long} {
				if _, ok := rules[recordName(w)]; !ok {
					t.Errorf("%s: alert %s reads %s, which is not recorded", in.Type, br.name, recordName(w))
				}
			}
		}
	}
}

// The budget window must be built from the recorded 5m rates, not a raw
// rate over weeks of samples.
func TestBudgetWindowUsesRecordedRates(t *testing.T) {
	for w := range validWindows {
		in := avail("api", "")
		in.Window = w
		s := mustSpec(t, in)
		r, ok := recordingRules(t, s)[recordName(w)]
		if !ok {
			t.Fatalf("window %s: no %s rule", w, recordName(w))
		}
		for _, want := range []string{"sum_over_time(" + recBad5m, "sum_over_time(" + recTotal5m, "[" + w + "]"} {
			if !strings.Contains(r.Expr, want) {
				t.Errorf("window %s: expr missing %q:\n%s", w, want, r.Expr)
			}
		}
		if strings.Contains(r.Expr, "requests_total") {
			t.Errorf("window %s: expr reads raw samples:\n%s", w, r.Expr)
		}
		dash, _ := generateGrafanaDashboard(s)
		if !strings.Contains(dash, recordName(w)) {
			t.Errorf("window %s: dashboard does not use %s", w, recordName(w))
		}
	}
}

func TestTwoSLOsForOneServiceDoNotClash(t *testing.T) {
	a := mustSpec(t, avail("checkout", "prometheus-http"))
	b := mustSpec(t, latency("checkout", "prometheus-http", "300"))
	if a.Name == b.Name {
		t.Fatalf("default names are equal: %q", a.Name)
	}
	ra, _ := generateRecordingRules(a)
	rb, _ := generateRecordingRules(b)
	if parseRules(t, ra).Groups[0].Name == parseRules(t, rb).Groups[0].Name {
		t.Errorf("rule groups share a name")
	}
	for _, s := range []Spec{a, b} {
		for name, r := range recordingRules(t, s) {
			if r.Labels["slo"] != s.Name || r.Labels["service"] != "checkout" {
				t.Errorf("%s/%s: labels = %v", s.Name, name, r.Labels)
			}
		}
		y, _ := generateAlertingRules(s)
		for _, r := range parseRules(t, y).Groups[0].Rules {
			if !strings.Contains(r.Expr, `slo="`+s.Name+`"`) {
				t.Errorf("%s: alert %s does not filter on slo:\n%s", s.Name, r.Alert, r.Expr)
			}
		}
	}
	if dashboardUID(a.Service, a.Name) == dashboardUID(b.Service, b.Name) {
		t.Errorf("dashboards share a UID")
	}
	ya, _ := generateAlertingRules(a)
	yb, _ := generateAlertingRules(b)
	if parseRules(t, ya).Groups[0].Rules[0].Alert == parseRules(t, yb).Groups[0].Rules[0].Alert {
		t.Errorf("alerts share a name")
	}
}

func TestHyphenatedServiceProducesValidNames(t *testing.T) {
	s := mustSpec(t, avail("payment-api", "service-counter"))
	if s.TotalSelector != "payment_api_requests_total" {
		t.Errorf("total selector = %q", s.TotalSelector)
	}
	y, _ := generateAlertingRules(s)
	for _, r := range parseRules(t, y).Groups[0].Rules {
		if !strings.HasPrefix(r.Alert, "PaymentApiAvailabilityErrorBudget") {
			t.Errorf("alert name = %q", r.Alert)
		}
	}
}

func TestMetricPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"payment-api": "payment_api",
		"svc.v2":      "svc_v2",
		"9lives":      "_9lives",
		"plain_name":  "plain_name",
	} {
		if got := metricPrefix(in); got != want {
			t.Errorf("metricPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPresetsResolveToValidSelectors(t *testing.T) {
	for name := range metricSources {
		a := mustSpec(t, avail("checkout", name))
		l := mustSpec(t, latency("checkout", name, "300"))
		for _, sel := range []string{a.BadSelector, a.TotalSelector, l.GoodSelector, l.TotalSelector} {
			if !selectorRe.MatchString(sel) {
				t.Errorf("%s: %q is not a valid selector", name, sel)
			}
			if !strings.Contains(sel, "checkout") {
				t.Errorf("%s: %q does not reference the service", name, sel)
			}
		}
		if !strings.Contains(l.GoodSelector, "_bucket") || !strings.Contains(l.GoodSelector, "le=") {
			t.Errorf("%s: good selector %q is not a bucket at the threshold", name, l.GoodSelector)
		}
		if !strings.Contains(l.TotalSelector, "_count") {
			t.Errorf("%s: latency total %q is not a histogram count", name, l.TotalSelector)
		}
	}
}

func TestLatencyThresholdUnits(t *testing.T) {
	if s := mustSpec(t, latency("api", "prometheus-http", "300")); !strings.Contains(s.GoodSelector, `le="0.3"`) {
		t.Errorf("seconds histogram: %s", s.GoodSelector)
	}
	if s := mustSpec(t, latency("api", "istio", "250")); !strings.Contains(s.GoodSelector, `le=~"250|250.0"`) {
		t.Errorf("milliseconds histogram: %s", s.GoodSelector)
	}
	if s := mustSpec(t, latency("api", "prometheus-http", "1000")); !strings.Contains(s.GoodSelector, `le=~"1|1.0"`) {
		t.Errorf("whole-second threshold: %s", s.GoodSelector)
	}
}

func TestLatencyBadRateIsCountMinusGood(t *testing.T) {
	s := mustSpec(t, latency("api", "prometheus-http", "300"))
	r := recordingRules(t, s)[recBad5m]
	want := `sum(rate(http_request_duration_seconds_count{job="api"}[5m])) - sum(rate(http_request_duration_seconds_bucket{job="api",le="0.3"}[5m]))`
	if r.Expr != want {
		t.Errorf("bad rate =\n%s\nwant\n%s", r.Expr, want)
	}
}

func TestCustomSelectors(t *testing.T) {
	in := avail("pay", "custom")
	in.ErrorSelector = `http_server_requests_seconds_count{app="pay",outcome="SERVER_ERROR"}`
	in.TotalSelector = `http_server_requests_seconds_count{app="pay"}`
	y, _ := generateRecordingRules(mustSpec(t, in))
	want := "sum(rate(" + in.ErrorSelector + "[5m]))"
	if !strings.Contains(y, want) {
		t.Errorf("recording rules missing %s:\n%s", want, y)
	}

	l := latency("pay", "custom", "500")
	l.BucketSelector = `http_server_requests_seconds_bucket{app="pay",module="web"}`
	s := mustSpec(t, l)
	if s.GoodSelector != `http_server_requests_seconds_bucket{app="pay",module="web",le="0.5"}` {
		t.Errorf("good selector = %s", s.GoodSelector)
	}
	if s.TotalSelector != `http_server_requests_seconds_count{app="pay",module="web"}` {
		t.Errorf("total selector = %s", s.TotalSelector)
	}
}

func TestWithMatcher(t *testing.T) {
	for sel, want := range map[string]string{
		"foo_bucket":        `foo_bucket{le="1"}`,
		"foo_bucket{}":      `foo_bucket{le="1"}`,
		`foo_bucket{a="b"}`: `foo_bucket{a="b",le="1"}`,
	} {
		if got := withMatcher(sel, `le="1"`); got != want {
			t.Errorf("withMatcher(%q) = %q, want %q", sel, got, want)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	ok := `x_total{a="b"}`
	cases := []struct {
		name    string
		in      Input
		wantErr string
	}{
		{"empty service", Input{SLO: "99.9"}, "service name required"},
		{"quote in service", Input{Service: `a"b`, SLO: "99.9"}, "service name must"},
		{"space in service", Input{Service: "a b", SLO: "99.9"}, "service name must"},
		{"slo too low", Input{Service: "a", SLO: "80"}, "SLO must be"},
		{"bad window", Input{Service: "a", SLO: "99.9", Window: "1y"}, "window must be"},
		{"bad type", Input{Service: "a", SLO: "99.9", Type: "throughput"}, "type must be"},
		{"bad slo name", Input{Service: "a", SLO: "99.9", Name: "Has Spaces"}, "SLO name must"},
		{"unknown source", Input{Service: "a", SLO: "99.9", Source: "datadog"}, "unknown metric source"},
		{"custom missing", Input{Service: "a", SLO: "99.9", Source: "custom", TotalSelector: ok}, "error selector is required"},
		{"custom with range", Input{Service: "a", SLO: "99.9", Source: "custom", ErrorSelector: "x_total[5m]", TotalSelector: ok}, "vector selector"},
		{"custom with function", Input{Service: "a", SLO: "99.9", Source: "custom", ErrorSelector: "rate(x_total[5m])", TotalSelector: ok}, "vector selector"},
		{"custom with newline", Input{Service: "a", SLO: "99.9", Source: "custom", ErrorSelector: ok, TotalSelector: "x_total{a=\"b\n\"}"}, "vector selector"},
		{"custom too long", Input{Service: "a", SLO: "99.9", Source: "custom", ErrorSelector: "x" + strings.Repeat("y", maxSelectorLen), TotalSelector: ok}, "at most"},
		{"latency no threshold", Input{Service: "a", SLO: "99", Type: "latency"}, "latency threshold"},
		{"latency huge threshold", Input{Service: "a", SLO: "99", Type: "latency", ThresholdMs: "90000"}, "latency threshold"},
		{"latency not a bucket", Input{Service: "a", SLO: "99", Type: "latency", ThresholdMs: "300", Source: "custom", BucketSelector: "foo_count"}, "ending in _bucket"},
		{"latency with le", Input{Service: "a", SLO: "99", Type: "latency", ThresholdMs: "300", Source: "custom", BucketSelector: `foo_bucket{le="0.3"}`}, "leave le out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newSpec(c.in)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func postMultipart(t *testing.T, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/generate", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	handleGenerate(rec, req)
	return rec
}

func TestHandleGenerate(t *testing.T) {
	rec := postMultipart(t, map[string]string{
		"service": "payment-api", "slo": "99.9", "window": "30d", "source": "grpc",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.Contains(out["recording_rules"], "grpc_server_handled_total") {
		t.Errorf("recording rules do not use the gRPC preset")
	}
	var dash map[string]any
	if err := json.Unmarshal([]byte(out["grafana_dashboard"]), &dash); err != nil {
		t.Errorf("dashboard is not valid JSON: %v", err)
	}
}

func TestHandleGenerateURLEncoded(t *testing.T) {
	form := url.Values{"service": {"api"}, "slo": {"99"}, "type": {"latency"}, "threshold_ms": {"200"}, "source": {"prometheus-http"}}
	req := httptest.NewRequest(http.MethodPost, "/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.2:1234"
	rec := httptest.NewRecorder()
	handleGenerate(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "http_request_duration_seconds_bucket") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestHandleGenerateReturnsJSONError(t *testing.T) {
	rec := postMultipart(t, map[string]string{"service": `bad"name`, "slo": "99.9"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["error"] == "" {
		t.Errorf("body is not a JSON error: %s", rec.Body)
	}
}

var grafanaUIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)

func TestDashboardUID(t *testing.T) {
	if got := dashboardUID("payment-api", "availability"); got != "slo-payment_api-availability" {
		t.Errorf("dashboardUID = %q", got)
	}
	long1 := strings.Repeat("a", 50) + "-one"
	long2 := strings.Repeat("a", 50) + "-two"
	u1, u2 := dashboardUID(long1, "latency"), dashboardUID(long2, "latency")
	for _, u := range []string{u1, u2, dashboardUID("svc.v2", "availability")} {
		if !grafanaUIDRe.MatchString(u) {
			t.Errorf("invalid Grafana UID %q", u)
		}
	}
	if u1 == u2 {
		t.Errorf("long names collide: %q", u1)
	}
	if dashboardUID(long1, "latency") != u1 {
		t.Errorf("UID is not stable")
	}
}

func TestDashboardIsGrafanaModel(t *testing.T) {
	for _, in := range []Input{avail("payment-api", ""), latency("payment-api", "", "300")} {
		out, err := generateGrafanaDashboard(mustSpec(t, in))
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			UID           string `json:"uid"`
			SchemaVersion int    `json:"schemaVersion"`
			Templating    struct {
				List []struct{ Name, Type, Query string } `json:"list"`
			} `json:"templating"`
			Panels []struct {
				ID         int               `json:"id"`
				Datasource map[string]string `json:"datasource"`
				Targets    []struct {
					RefID      string            `json:"refId"`
					Expr       string            `json:"expr"`
					Datasource map[string]string `json:"datasource"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatal(err)
		}
		if d.UID == "" || d.SchemaVersion != grafanaSchemaVersion {
			t.Errorf("uid=%q schemaVersion=%d", d.UID, d.SchemaVersion)
		}
		if len(d.Templating.List) != 1 || d.Templating.List[0].Type != "datasource" || d.Templating.List[0].Query != "prometheus" {
			t.Errorf("missing prometheus datasource variable: %+v", d.Templating.List)
		}
		ids := map[int]bool{}
		for _, p := range d.Panels {
			if p.ID == 0 || ids[p.ID] {
				t.Errorf("panel id %d missing or duplicated", p.ID)
			}
			ids[p.ID] = true
			if p.Datasource["uid"] != "${datasource}" {
				t.Errorf("panel %d datasource = %v", p.ID, p.Datasource)
			}
			for _, tg := range p.Targets {
				if tg.RefID == "" || tg.Datasource["uid"] != "${datasource}" {
					t.Errorf("panel %d target missing refId or datasource", p.ID)
				}
				if !strings.Contains(tg.Expr, `slo="`) {
					t.Errorf("panel %d query does not filter on slo: %s", p.ID, tg.Expr)
				}
			}
		}
	}
}

func TestCLIWritesFiles(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{"-service", "checkout-api", "-slo", "99.9", "-source", "prometheus-http", "-out", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, f := range []string{"checkout-api-availability-recording.yaml", "checkout-api-availability-alerting.yaml", "checkout-api-availability-dashboard.json"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || len(b) == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestCLIJSONAndErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"-service", "api", "-slo", "99", "-type", "latency", "-threshold_ms", "300", "-out", "-"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var a Artifacts
	if err := json.Unmarshal(stdout.Bytes(), &a); err != nil || a.RecordingRules == "" {
		t.Errorf("stdout is not artifacts JSON: %v", err)
	}
	stderr.Reset()
	if code := runCLI([]string{"-service", "api", "-slo", "50"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "SLO must be") {
		t.Errorf("bad input: exit %d, stderr %q", code, stderr.String())
	}
}
