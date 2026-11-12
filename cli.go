package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const cliUsage = `Usage: ithildin generate [flags]

Generates Prometheus recording rules, alerting rules and a Grafana dashboard
for one SLO. Flags take the same values as the web form.

Examples:
  ithildin generate -service checkout-api -slo 99.9 -source prometheus-http
  ithildin generate -service checkout-api -name fast-checkout -type latency \
      -threshold_ms 300 -slo 99 -source prometheus-http -out rules/

Flags:
`

// runCLI implements `ithildin generate` and returns the exit code.
func runCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, cliUsage)
		fs.PrintDefaults()
	}
	var in Input
	fs.StringVar(&in.Service, "service", "", "service name (required)")
	fs.StringVar(&in.SLO, "slo", "", "SLO target in percent, 90 to 99.999 (required)")
	fs.StringVar(&in.Name, "name", "", "SLO name, to tell several SLOs for one service apart (default: the type)")
	fs.StringVar(&in.Type, "type", "", "availability or latency (default availability)")
	fs.StringVar(&in.Window, "window", "", "error budget window: 7d, 30d or 90d (default 30d)")
	fs.StringVar(&in.Source, "source", "", "metric source: service-counter, prometheus-http, grpc, istio, ingress-nginx or custom (default service-counter)")
	fs.StringVar(&in.ThresholdMs, "threshold_ms", "", "latency threshold in milliseconds (latency only)")
	fs.StringVar(&in.ErrorSelector, "error_selector", "", "failed requests selector (custom availability)")
	fs.StringVar(&in.TotalSelector, "total_selector", "", "all requests selector (custom availability)")
	fs.StringVar(&in.BucketSelector, "bucket_selector", "", "histogram _bucket selector (custom latency)")
	out := fs.String("out", ".", `directory to write files to, or "-" to print JSON to stdout`)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	spec, err := newSpec(in)
	if err != nil {
		fmt.Fprintln(stderr, "ithildin:", err)
		return 1
	}
	a, err := generate(spec)
	if err != nil {
		fmt.Fprintln(stderr, "ithildin:", err)
		return 1
	}

	if *out == "-" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(a); err != nil {
			fmt.Fprintln(stderr, "ithildin:", err)
			return 1
		}
		return 0
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(stderr, "ithildin:", err)
		return 1
	}
	base := spec.Service + "-" + spec.Name
	files := []struct{ name, body string }{
		{base + "-recording.yaml", a.RecordingRules},
		{base + "-alerting.yaml", a.AlertingRules},
		{base + "-dashboard.json", a.GrafanaDashboard},
	}
	for _, f := range files {
		path := filepath.Join(*out, f.name)
		if err := os.WriteFile(path, []byte(f.body), 0o644); err != nil {
			fmt.Fprintln(stderr, "ithildin:", err)
			return 1
		}
		fmt.Fprintln(stdout, path)
	}
	return 0
}
