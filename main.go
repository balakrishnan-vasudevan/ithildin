package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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
		return strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
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

// Rule generators

func generateRecordingRules(service string, slo float64) (string, error) {
	windows := []string{"5m", "30m", "1h", "6h"}
	rules := make([]Rule, len(windows))
	for i, w := range windows {
		rules[i] = Rule{
			Record: fmt.Sprintf("job:slo_errors:rate%s", w),
			Expr: fmt.Sprintf(
				`sum(rate(%s_requests_total{status=~"5.."}[%s])) / sum(rate(%s_requests_total[%s]))`,
				service, w, service, w,
			),
			Labels: map[string]string{"service": service},
		}
	}
	out := PrometheusRules{Groups: []RuleGroup{{
		Name:     fmt.Sprintf("%s.slo.rules", service),
		Interval: "30s",
		Rules:    rules,
	}}}
	b, err := yaml.Marshal(out)
	return string(b), err
}

type burnRate struct {
	name, severity, short, long, forDur string
	burn                                 float64
}

var burnRates = []burnRate{
	{"CriticalFast", "critical", "5m", "1h", "2m", 14.4},
	{"CriticalSlow", "critical", "30m", "6h", "15m", 6},
	{"WarningFast", "warning", "1h", "1d", "1h", 3},
	{"WarningSlow", "warning", "6h", "3d", "3h", 1},
}

func generateAlertingRules(service string, slo float64) (string, error) {
	errorRate := 1 - slo/100
	rules := make([]Rule, len(burnRates))
	for i, br := range burnRates {
		threshold := errorRate * br.burn
		rules[i] = Rule{
			Alert: fmt.Sprintf("%sErrorBudget%s", service, br.name),
			Expr: fmt.Sprintf(
				"job:slo_errors:rate%s{service=%q} > %.6f\nand\njob:slo_errors:rate%s{service=%q} > %.6f",
				br.short, service, threshold, br.long, service, threshold,
			),
			For:    br.forDur,
			Labels: map[string]string{"severity": br.severity, "service": service},
			Annotations: map[string]string{
				"summary": fmt.Sprintf("%s error budget burn rate too high (%s)", service, br.name),
				"description": fmt.Sprintf(
					"Error budget for %s (SLO: %.3f%%) is being consumed at %.1fx the acceptable rate. Short window (%s): {{ $value | humanizePercentage }}.",
					service, slo, br.burn, br.short,
				),
			},
		}
	}
	out := PrometheusRules{Groups: []RuleGroup{{
		Name:  fmt.Sprintf("%s.slo.alerts", service),
		Rules: rules,
	}}}
	b, err := yaml.Marshal(out)
	return string(b), err
}

func generateGrafanaDashboard(service string, slo float64) (string, error) {
	dashboard := map[string]any{
		"title": fmt.Sprintf("%s SLO Dashboard", service),
		"tags":  []string{"slo", "observability", "ithildin"},
		"panels": []map[string]any{
			{
				"title":   "Error Rate",
				"type":    "timeseries",
				"gridPos": map[string]int{"h": 8, "w": 12, "x": 0, "y": 0},
				"targets": []map[string]string{{
					"expr":         fmt.Sprintf(`job:slo_errors:rate5m{service="%s"}`, service),
					"legendFormat": "5m error rate",
				}},
			},
			{
				"title":   fmt.Sprintf("SLO Compliance (%.3f%%)", slo),
				"type":    "gauge",
				"gridPos": map[string]int{"h": 8, "w": 12, "x": 12, "y": 0},
				"targets": []map[string]string{{
					"expr":         fmt.Sprintf(`1 - job:slo_errors:rate5m{service="%s"}`, service),
					"legendFormat": "availability",
				}},
				"fieldConfig": map[string]any{
					"defaults": map[string]any{
						"thresholds": map[string]any{
							"steps": []map[string]any{
								{"color": "red", "value": 0},
								{"color": "yellow", "value": slo/100 - 0.001},
								{"color": "green", "value": slo / 100},
							},
						},
					},
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

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !getLimiter(ip).Allow() {
		http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"invalid form"}`, http.StatusBadRequest)
		return
	}

	service := strings.TrimSpace(r.FormValue("service"))
	sloStr := r.FormValue("slo")
	window := r.FormValue("window")
	if window == "" {
		window = "30d"
	}

	if service == "" {
		http.Error(w, `{"error":"service name required"}`, http.StatusBadRequest)
		return
	}
	slo, err := strconv.ParseFloat(sloStr, 64)
	if err != nil || slo < 90 || slo > 99.999 {
		http.Error(w, `{"error":"SLO must be between 90 and 99.999"}`, http.StatusBadRequest)
		return
	}

	slog.Info("generate", "service", service, "slo", slo, "window", window, "ip", ip)

	recording, err := generateRecordingRules(service, slo)
	if err != nil {
		http.Error(w, `{"error":"generation failed"}`, http.StatusInternalServerError)
		return
	}
	alerting, err := generateAlertingRules(service, slo)
	if err != nil {
		http.Error(w, `{"error":"generation failed"}`, http.StatusInternalServerError)
		return
	}
	dashboard, err := generateGrafanaDashboard(service, slo)
	if err != nil {
		http.Error(w, `{"error":"generation failed"}`, http.StatusInternalServerError)
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
