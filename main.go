package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

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

	// Accept both multipart (the UI) and urlencoded (curl -d) bodies.
	err := r.ParseMultipartForm(1 << 20)
	if err == http.ErrNotMultipart {
		err = r.ParseForm()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return
	}

	field := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	spec, err := newSpec(Input{
		Service:        field("service"),
		Name:           field("name"),
		Type:           field("type"),
		SLO:            field("slo"),
		Window:         field("window"),
		Source:         field("source"),
		ThresholdMs:    field("threshold_ms"),
		ErrorSelector:  field("error_selector"),
		TotalSelector:  field("total_selector"),
		BucketSelector: field("bucket_selector"),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	slog.Info("generate", "service", spec.Service, "name", spec.Name, "type", spec.Type,
		"slo", spec.SLO, "window", spec.Window, "source", field("source"), "ip", ip)

	artifacts, err := generate(spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generation failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(artifacts)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "generate" {
		os.Exit(runCLI(os.Args[2:], os.Stdout, os.Stderr))
	}

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
