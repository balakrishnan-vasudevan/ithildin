package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
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

// Per-IP rate limiting

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

var (
	mu       sync.Mutex
	visitors = make(map[string]*visitor)
)

func getLimiter(ip string) *rate.Limiter {
	mu.Lock()
	defer mu.Unlock()
	v, ok := visitors[ip]
	if !ok {
		v = &visitor{limiter: rate.NewLimiter(rate.Every(time.Minute/20), 20)}
		visitors[ip] = v
	}
	v.lastSeen = time.Now()
	return v.limiter
}

func cleanupVisitors() {
	for range time.Tick(5 * time.Minute) {
		mu.Lock()
		for ip, v := range visitors {
			if time.Since(v.lastSeen) > 10*time.Minute {
				delete(visitors, ip)
			}
		}
		mu.Unlock()
	}
}

// clientIP extracts the real client IP, accounting for Render's proxy.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Service spec

// Spec is a validated service definition that every artifact is generated from.
type Spec struct {
	Service       string  // value of the `service` label; also used in names
	SLO           float64 // percent, e.g. 99.9
	Window        string  // error budget window, e.g. "30d"
	ErrorSelector string  // PromQL vector selector matching failed requests
	TotalSelector string  // PromQL vector selector matching all requests
}

var (
	// Restricted so the name is always safe inside PromQL label matchers.
	serviceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	// A plain vector selector: optional metric name plus optional label
	// matchers. Range brackets are rejected because we add them ourselves.
	selectorRe   = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)?(\{[^{}\[\]\n]*\})?$`)
	validWindows = map[string]bool{"7d": true, "30d": true, "90d": true}
)

const maxSelectorLen = 512

// metricPrefix turns a service name into a valid Prometheus metric name
// prefix: "payment-api" -> "payment_api".
func metricPrefix(service string) string {
	b := []byte(service)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			b[i] = '_'
		}
	}
	if b[0] >= '0' && b[0] <= '9' {
		return "_" + string(b)
	}
	return string(b)
}

// alertPrefix turns a service name into a CamelCase alert name prefix:
// "payment-api" -> "PaymentApi".
func alertPrefix(service string) string {
	parts := strings.FieldsFunc(service, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
	})
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return sb.String()
}

// Metric sources

// metricSource maps a common instrumentation convention to error/total
// selectors. %[1]s is the service label value, %[2]s the metric prefix.
type metricSource struct {
	errors, total string
}

const defaultSource = "service-counter"

var metricSources = map[string]metricSource{
	// <service>_requests_total{status="..."}: Ithildin's original convention.
	"service-counter": {
		errors: `%[2]s_requests_total{status=~"5.."}`,
		total:  `%[2]s_requests_total`,
	},
	// Prometheus client / promhttp middleware, scraped under job=<service>.
	"prometheus-http": {
		errors: `http_requests_total{job="%[1]s",code=~"5.."}`,
		total:  `http_requests_total{job="%[1]s"}`,
	},
	// go-grpc-prometheus / grpc-ecosystem server metrics. Only server-side
	// failure codes count against the SLO, not client errors like NotFound.
	"grpc": {
		errors: `grpc_server_handled_total{job="%[1]s",grpc_code=~"Unknown|DeadlineExceeded|Unimplemented|Internal|Unavailable|DataLoss"}`,
		total:  `grpc_server_handled_total{job="%[1]s"}`,
	},
	// Istio standard metrics, measured at the destination sidecar.
	"istio": {
		errors: `istio_requests_total{reporter="destination",destination_service_name="%[1]s",response_code=~"5.."}`,
		total:  `istio_requests_total{reporter="destination",destination_service_name="%[1]s"}`,
	},
	// ingress-nginx controller, keyed by backend Kubernetes service.
	"ingress-nginx": {
		errors: `nginx_ingress_controller_requests{service="%[1]s",status=~"5.."}`,
		total:  `nginx_ingress_controller_requests{service="%[1]s"}`,
	},
}

func validateSelector(field, sel string) error {
	if sel == "" {
		return fmt.Errorf("%s is required for a custom metric source", field)
	}
	if len(sel) > maxSelectorLen {
		return fmt.Errorf("%s must be at most %d characters", field, maxSelectorLen)
	}
	if !selectorRe.MatchString(sel) {
		return fmt.Errorf(`%s must be a vector selector like metric_name{label="value"} (no range or functions)`, field)
	}
	return nil
}

// newSpec validates raw form input and resolves the metric selectors.
func newSpec(service, sloStr, window, source, errSel, totalSel string) (Spec, error) {
	if service == "" {
		return Spec{}, fmt.Errorf("service name required")
	}
	if !serviceRe.MatchString(service) {
		return Spec{}, fmt.Errorf("service name must start with a letter or digit and contain only letters, digits, '-', '_' or '.' (max 63)")
	}
	slo, err := strconv.ParseFloat(sloStr, 64)
	if err != nil || slo < 90 || slo > 99.999 {
		return Spec{}, fmt.Errorf("SLO must be between 90 and 99.999")
	}
	if window == "" {
		window = "30d"
	}
	if !validWindows[window] {
		return Spec{}, fmt.Errorf("window must be one of 7d, 30d, 90d")
	}

	spec := Spec{Service: service, SLO: slo, Window: window}
	if source == "" {
		source = defaultSource
	}
	if source == "custom" {
		if err := validateSelector("error selector", errSel); err != nil {
			return Spec{}, err
		}
		if err := validateSelector("total selector", totalSel); err != nil {
			return Spec{}, err
		}
		spec.ErrorSelector, spec.TotalSelector = errSel, totalSel
		return spec, nil
	}
	ms, ok := metricSources[source]
	if !ok {
		return Spec{}, fmt.Errorf("unknown metric source %q", source)
	}
	prefix := metricPrefix(service)
	spec.ErrorSelector = fmt.Sprintf(ms.errors, service, prefix)
	spec.TotalSelector = fmt.Sprintf(ms.total, service, prefix)
	return spec, nil
}

// Rule generators

// alertWindows must cover every short/long window used in burnRates.
var alertWindows = []string{"5m", "30m", "1h", "6h", "1d", "3d"}

func recordName(window string) string {
	return "job:slo_errors:rate" + window
}

func errorRatioRule(s Spec, window string) Rule {
	return Rule{
		Record: recordName(window),
		Expr: fmt.Sprintf("sum(rate(%s[%s]))\n/\nsum(rate(%s[%s]))",
			s.ErrorSelector, window, s.TotalSelector, window),
		Labels: map[string]string{"service": s.Service},
	}
}

func generateRecordingRules(s Spec) (string, error) {
	rules := make([]Rule, len(alertWindows))
	for i, w := range alertWindows {
		rules[i] = errorRatioRule(s, w)
	}
	out := PrometheusRules{Groups: []RuleGroup{
		{
			Name:     fmt.Sprintf("%s.slo.rules", s.Service),
			Interval: "30s",
			Rules:    rules,
		},
		{
			// The full budget window scans weeks of samples, so evaluate it
			// far less often than the alerting windows.
			Name:     fmt.Sprintf("%s.slo.window", s.Service),
			Interval: "5m",
			Rules:    []Rule{errorRatioRule(s, s.Window)},
		},
	}}
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
		rules[i] = Rule{
			Alert: fmt.Sprintf("%sErrorBudget%s", alertPrefix(s.Service), br.name),
			Expr: fmt.Sprintf(
				"%s{service=%q} > %.6f\nand\n%s{service=%q} > %.6f",
				recordName(br.short), s.Service, threshold, recordName(br.long), s.Service, threshold,
			),
			For:    br.forDur,
			Labels: map[string]string{"severity": br.severity, "service": s.Service},
			Annotations: map[string]string{
				"summary": fmt.Sprintf("%s error budget burn rate too high (%s)", s.Service, br.name),
				"description": fmt.Sprintf(
					"Error budget for %s (SLO: %.3f%% over %s) is being consumed at %.1fx the acceptable rate. Short window (%s): {{ $value | humanizePercentage }}.",
					s.Service, s.SLO, s.Window, br.burn, br.short,
				),
			},
		}
	}
	out := PrometheusRules{Groups: []RuleGroup{{
		Name:  fmt.Sprintf("%s.slo.alerts", s.Service),
		Rules: rules,
	}}}
	b, err := yaml.Marshal(out)
	return string(b), err
}

// Dashboard schema version 39 is what Grafana 10.4 writes natively, so it
// imports there with no migration. Newer versions upgrade it on load.
const grafanaSchemaVersion = 39

// dashboardUID derives a stable UID from the service name, so re-importing
// a regenerated dashboard updates the existing one instead of duplicating it.
// Grafana UIDs are limited to 40 characters.
func dashboardUID(service string) string {
	uid := "slo-" + metricPrefix(service)
	if len(uid) <= 40 {
		return uid
	}
	h := fnv.New32a()
	h.Write([]byte(service))
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
	dashboard := map[string]any{
		"uid":           dashboardUID(s.Service),
		"title":         fmt.Sprintf("%s SLO Dashboard", s.Service),
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
				"title":      "Error Rate",
				"type":       "timeseries",
				"datasource": promDatasource,
				"gridPos":    map[string]int{"h": 8, "w": 12, "x": 0, "y": 0},
				"targets": promTarget(
					fmt.Sprintf(`%s{service=%q}`, recordName("5m"), s.Service),
					"5m error rate",
				),
				"fieldConfig": map[string]any{
					"defaults":  map[string]any{"unit": "percentunit"},
					"overrides": []any{},
				},
			},
			{
				"id":         2,
				"title":      fmt.Sprintf("Availability over %s (SLO %.3f%%)", s.Window, s.SLO),
				"type":       "gauge",
				"datasource": promDatasource,
				"gridPos":    map[string]int{"h": 8, "w": 12, "x": 12, "y": 0},
				"targets": promTarget(
					fmt.Sprintf(`1 - %s{service=%q}`, recordName(s.Window), s.Service),
					"availability",
				),
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

// Logging middleware

var skipPaths = map[string]bool{
	"/favicon.ico":                      true,
	"/apple-touch-icon.png":             true,
	"/apple-touch-icon-precomposed.png": true,
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(status int) {
	sr.status = status
	sr.ResponseWriter.WriteHeader(status)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"ip", clientIP(r),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// Handlers

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "templates/index.html")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !getLimiter(ip).Allow() {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return
	}

	spec, err := newSpec(
		strings.TrimSpace(r.FormValue("service")),
		r.FormValue("slo"),
		r.FormValue("window"),
		r.FormValue("source"),
		strings.TrimSpace(r.FormValue("error_selector")),
		strings.TrimSpace(r.FormValue("total_selector")),
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	slog.Info("generate", "service", spec.Service, "slo", spec.SLO, "window", spec.Window,
		"source", r.FormValue("source"), "ip", ip)

	recording, err := generateRecordingRules(spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generation failed")
		return
	}
	alerting, err := generateAlertingRules(spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generation failed")
		return
	}
	dashboard, err := generateGrafanaDashboard(spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generation failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"recording_rules":   recording,
		"alerting_rules":    alerting,
		"grafana_dashboard": dashboard,
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	go cleanupVisitors()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("POST /generate", handleGenerate)

	slog.Info("starting", "port", port)
	if err := http.ListenAndServe(":"+port, withLogging(mux)); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
