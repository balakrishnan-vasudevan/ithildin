package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Input is the raw service definition, as it arrives from the web form,
// the query string or CLI flags. Field names match the form fields.
type Input struct {
	Service        string // service
	Name           string // name: SLO name, defaults to the SLO type
	Type           string // type: "availability" (default) or "latency"
	SLO            string // slo: target percentage
	Window         string // window: error budget window
	Source         string // source: metric source preset or "custom"
	ThresholdMs    string // threshold_ms: latency threshold in milliseconds
	ErrorSelector  string // error_selector: custom availability
	TotalSelector  string // total_selector: custom availability
	BucketSelector string // bucket_selector: custom latency histogram
}

// Spec is a validated SLO definition that every artifact is generated from.
//
// Every SLO is reduced to two counters, "bad" and "total" events:
//   - availability: bad = failed requests, total = all requests
//   - latency: total = histogram _count, good = the bucket at the threshold,
//     bad = total - good
type Spec struct {
	Service     string  // value of the `service` label
	Name        string  // value of the `slo` label
	Type        string  // "availability" or "latency"
	SLO         float64 // percent, e.g. 99.9
	Window      string  // error budget window, e.g. "30d"
	ThresholdMs float64 // latency only

	BadSelector   string // availability: failed requests
	GoodSelector  string // latency: requests at or under the threshold
	TotalSelector string // all requests
}

const (
	typeAvailability = "availability"
	typeLatency      = "latency"
	defaultSource    = "service-counter"
	maxSelectorLen   = 512
)

var (
	// Restricted so the name is always safe inside PromQL label matchers.
	serviceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	sloNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// A plain vector selector: optional metric name plus optional label
	// matchers. Range brackets are rejected because we add them ourselves.
	selectorRe   = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)?(\{[^{}\[\]\n]*\})?$`)
	bucketNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*_bucket(\{|$)`)
	leLabelRe    = regexp.MustCompile(`[{,]\s*le\s*[=!]`)
	validWindows = map[string]bool{"7d": true, "30d": true, "90d": true}
)

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

// camel turns a name into CamelCase for alert names: "payment-api" -> "PaymentApi".
func camel(s string) string {
	parts := strings.FieldsFunc(s, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
	})
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return sb.String()
}

// Metric sources

// metricSource maps a common instrumentation convention to selectors.
// %[1]s is the service label value, %[2]s the metric prefix.
type metricSource struct {
	errors, total string // availability
	bucket        string // latency: histogram _bucket selector
	bucketMs      bool   // histogram buckets are in milliseconds, not seconds
}

var metricSources = map[string]metricSource{
	// <service>_requests_total{status="..."}: Ithildin's original convention.
	"service-counter": {
		errors: `%[2]s_requests_total{status=~"5.."}`,
		total:  `%[2]s_requests_total`,
		bucket: `%[2]s_request_duration_seconds_bucket`,
	},
	// Prometheus client / promhttp middleware, scraped under job=<service>.
	"prometheus-http": {
		errors: `http_requests_total{job="%[1]s",code=~"5.."}`,
		total:  `http_requests_total{job="%[1]s"}`,
		bucket: `http_request_duration_seconds_bucket{job="%[1]s"}`,
	},
	// go-grpc-prometheus / grpc-ecosystem server metrics. Only server-side
	// failure codes count against the SLO, not client errors like NotFound.
	// Latency needs handling-time histograms enabled on the server.
	"grpc": {
		errors: `grpc_server_handled_total{job="%[1]s",grpc_code=~"Unknown|DeadlineExceeded|Unimplemented|Internal|Unavailable|DataLoss"}`,
		total:  `grpc_server_handled_total{job="%[1]s"}`,
		bucket: `grpc_server_handling_seconds_bucket{job="%[1]s"}`,
	},
	// Istio standard metrics, measured at the destination sidecar.
	"istio": {
		errors:   `istio_requests_total{reporter="destination",destination_service_name="%[1]s",response_code=~"5.."}`,
		total:    `istio_requests_total{reporter="destination",destination_service_name="%[1]s"}`,
		bucket:   `istio_request_duration_milliseconds_bucket{reporter="destination",destination_service_name="%[1]s"}`,
		bucketMs: true,
	},
	// ingress-nginx controller, keyed by backend Kubernetes service.
	"ingress-nginx": {
		errors: `nginx_ingress_controller_requests{service="%[1]s",status=~"5.."}`,
		total:  `nginx_ingress_controller_requests{service="%[1]s"}`,
		bucket: `nginx_ingress_controller_request_duration_seconds_bucket{service="%[1]s"}`,
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

// leMatcher matches a histogram bucket boundary. Exporters write whole
// numbers as "1" while Prometheus 3 normalises them to "1.0", so accept both.
func leMatcher(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		return fmt.Sprintf(`le=~"%s|%s.0"`, s, s)
	}
	return fmt.Sprintf(`le="%s"`, s)
}

// withMatcher adds a label matcher to a vector selector.
func withMatcher(sel, matcher string) string {
	switch {
	case strings.HasSuffix(sel, "{}"):
		return strings.TrimSuffix(sel, "}") + matcher + "}"
	case strings.HasSuffix(sel, "}"):
		return strings.TrimSuffix(sel, "}") + "," + matcher + "}"
	default:
		return sel + "{" + matcher + "}"
	}
}

// countSelector turns a histogram _bucket selector into its _count selector.
func countSelector(bucket string) string {
	name, rest := bucket, ""
	if i := strings.Index(bucket, "{"); i >= 0 {
		name, rest = bucket[:i], bucket[i:]
	}
	return strings.TrimSuffix(name, "_bucket") + "_count" + rest
}

// newSpec validates raw input and resolves the metric selectors.
func newSpec(in Input) (Spec, error) {
	if in.Service == "" {
		return Spec{}, fmt.Errorf("service name required")
	}
	if !serviceRe.MatchString(in.Service) {
		return Spec{}, fmt.Errorf("service name must start with a letter or digit and contain only letters, digits, '-', '_' or '.' (max 63)")
	}
	slo, err := strconv.ParseFloat(in.SLO, 64)
	if err != nil || slo < 90 || slo > 99.999 {
		return Spec{}, fmt.Errorf("SLO must be between 90 and 99.999")
	}
	window := in.Window
	if window == "" {
		window = "30d"
	}
	if !validWindows[window] {
		return Spec{}, fmt.Errorf("window must be one of 7d, 30d, 90d")
	}
	typ := in.Type
	if typ == "" {
		typ = typeAvailability
	}
	if typ != typeAvailability && typ != typeLatency {
		return Spec{}, fmt.Errorf("type must be availability or latency")
	}
	name := in.Name
	if name == "" {
		name = typ
	}
	if !sloNameRe.MatchString(name) {
		return Spec{}, fmt.Errorf("SLO name must be lowercase letters, digits, '-' or '_' (max 32)")
	}
	source := in.Source
	if source == "" {
		source = defaultSource
	}

	spec := Spec{Service: in.Service, Name: name, Type: typ, SLO: slo, Window: window}
	if typ == typeAvailability {
		return spec.withAvailability(source, in)
	}
	ms, err := strconv.ParseFloat(in.ThresholdMs, 64)
	if err != nil || ms < 1 || ms > 60000 {
		return Spec{}, fmt.Errorf("latency threshold must be between 1 and 60000 ms")
	}
	spec.ThresholdMs = ms
	return spec.withLatency(source, in)
}

func (s Spec) withAvailability(source string, in Input) (Spec, error) {
	if source == "custom" {
		if err := validateSelector("error selector", in.ErrorSelector); err != nil {
			return Spec{}, err
		}
		if err := validateSelector("total selector", in.TotalSelector); err != nil {
			return Spec{}, err
		}
		s.BadSelector, s.TotalSelector = in.ErrorSelector, in.TotalSelector
		return s, nil
	}
	ms, ok := metricSources[source]
	if !ok {
		return Spec{}, fmt.Errorf("unknown metric source %q", source)
	}
	prefix := metricPrefix(s.Service)
	s.BadSelector = fmt.Sprintf(ms.errors, s.Service, prefix)
	s.TotalSelector = fmt.Sprintf(ms.total, s.Service, prefix)
	return s, nil
}

func (s Spec) withLatency(source string, in Input) (Spec, error) {
	var bucket string
	le := s.ThresholdMs / 1000
	if source == "custom" {
		if err := validateSelector("bucket selector", in.BucketSelector); err != nil {
			return Spec{}, err
		}
		if !bucketNameRe.MatchString(in.BucketSelector) {
			return Spec{}, fmt.Errorf("bucket selector must name a histogram metric ending in _bucket")
		}
		if leLabelRe.MatchString(in.BucketSelector) {
			return Spec{}, fmt.Errorf("leave le out of the bucket selector, it is set from the threshold")
		}
		bucket = in.BucketSelector
	} else {
		ms, ok := metricSources[source]
		if !ok {
			return Spec{}, fmt.Errorf("unknown metric source %q", source)
		}
		bucket = fmt.Sprintf(ms.bucket, s.Service, metricPrefix(s.Service))
		if ms.bucketMs {
			le = s.ThresholdMs
		}
	}
	s.GoodSelector = withMatcher(bucket, leMatcher(le))
	s.TotalSelector = countSelector(bucket)
	return s, nil
}

// Describe is a one-line human description of the objective.
func (s Spec) Describe() string {
	if s.Type == typeLatency {
		return fmt.Sprintf("%g%% of requests under %gms over %s", s.SLO, s.ThresholdMs, s.Window)
	}
	return fmt.Sprintf("%g%% of requests succeed over %s", s.SLO, s.Window)
}
