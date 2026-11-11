package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustSpec(t *testing.T, service, source string) Spec {
	t.Helper()
	s, err := newSpec(service, "99.9", "30d", source, "", "")
	if err != nil {
		t.Fatalf("newSpec(%q, %q): %v", service, source, err)
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

func recordedNames(t *testing.T, s Spec) map[string]bool {
	t.Helper()
	y, err := generateRecordingRules(s)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, g := range parseRules(t, y).Groups {
		for _, r := range g.Rules {
			names[r.Record] = true
		}
	}
	return names
}

// Every window an alert reads must have a recording rule, or the alert
// silently never fires.
func TestAlertWindowsAreRecorded(t *testing.T) {
	names := recordedNames(t, mustSpec(t, "payment-api", ""))
	for _, br := range burnRates {
		for _, w := range []string{br.short, br.long} {
			if !names[recordName(w)] {
				t.Errorf("alert %s reads %s, which is not recorded", br.name, recordName(w))
			}
		}
	}
}

func TestBudgetWindowIsRecordedAndUsed(t *testing.T) {
	for w := range validWindows {
		s, err := newSpec("api", "99.9", w, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if !recordedNames(t, s)[recordName(w)] {
			t.Errorf("window %s: no %s rule", w, recordName(w))
		}
		dash, _ := generateGrafanaDashboard(s)
		if !strings.Contains(dash, recordName(w)) {
			t.Errorf("window %s: dashboard does not use %s", w, recordName(w))
		}
	}
}

func TestHyphenatedServiceProducesValidNames(t *testing.T) {
	s := mustSpec(t, "payment-api", "service-counter")
	if s.TotalSelector != "payment_api_requests_total" {
		t.Errorf("total selector = %q", s.TotalSelector)
	}
	y, _ := generateAlertingRules(s)
	for _, r := range parseRules(t, y).Groups[0].Rules {
		if !strings.HasPrefix(r.Alert, "PaymentApiErrorBudget") {
			t.Errorf("alert name = %q", r.Alert)
		}
		if r.Labels["service"] != "payment-api" {
			t.Errorf("service label = %q", r.Labels["service"])
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
		s := mustSpec(t, "checkout", name)
		for _, sel := range []string{s.ErrorSelector, s.TotalSelector} {
			if !selectorRe.MatchString(sel) {
				t.Errorf("%s: %q is not a valid selector", name, sel)
			}
			if !strings.Contains(sel, "checkout") {
				t.Errorf("%s: %q does not reference the service", name, sel)
			}
		}
		y, _ := generateRecordingRules(s)
		if !strings.Contains(y, s.ErrorSelector) {
			t.Errorf("%s: recording rules do not use the error selector", name)
		}
	}
}

func TestCustomSelectors(t *testing.T) {
	errSel := `http_server_requests_seconds_count{app="pay",outcome="SERVER_ERROR"}`
	totalSel := `http_server_requests_seconds_count{app="pay"}`
	s, err := newSpec("pay", "99.5", "7d", "custom", errSel, totalSel)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := generateRecordingRules(s)
	want := "sum(rate(" + errSel + "[5m]))"
	if !strings.Contains(y, want) {
		t.Errorf("recording rules missing %s:\n%s", want, y)
	}
}

func TestValidationErrors(t *testing.T) {
	ok := `x_total{a="b"}`
	cases := []struct {
		name                                           string
		service, slo, window, source, errSel, totalSel string
		wantErr                                        string
	}{
		{"empty service", "", "99.9", "", "", "", "", "service name required"},
		{"quote in service", `a"b`, "99.9", "", "", "", "", "service name must"},
		{"space in service", "a b", "99.9", "", "", "", "", "service name must"},
		{"slo too low", "a", "80", "", "", "", "", "SLO must be"},
		{"bad window", "a", "99.9", "1y", "", "", "", "window must be"},
		{"unknown source", "a", "99.9", "", "datadog", "", "", "unknown metric source"},
		{"custom missing", "a", "99.9", "", "custom", "", ok, "error selector is required"},
		{"custom with range", "a", "99.9", "", "custom", "x_total[5m]", ok, "vector selector"},
		{"custom with function", "a", "99.9", "", "custom", "rate(x_total[5m])", ok, "vector selector"},
		{"custom with newline", "a", "99.9", "", "custom", ok, "x_total{a=\"b\n\"}", "vector selector"},
		{"custom too long", "a", "99.9", "", "custom", "x" + strings.Repeat("y", maxSelectorLen), ok, "at most"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newSpec(c.service, c.slo, c.window, c.source, c.errSel, c.totalSel)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func postForm(t *testing.T, fields map[string]string) *httptest.ResponseRecorder {
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
	rec := postForm(t, map[string]string{
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

func TestHandleGenerateReturnsJSONError(t *testing.T) {
	rec := postForm(t, map[string]string{"service": `bad"name`, "slo": "99.9"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["error"] == "" {
		t.Errorf("body is not a JSON error: %s", rec.Body)
	}
}
