# Ithildin

> *The moon-letters reveal what is hidden*

Ithildin is an Observability as Code tool. You describe an SLO (a service, a target, and whether it's about errors or latency) and it generates the Prometheus rules and Grafana dashboard for it.

Live at: https://ithildin.onrender.com

## What it does

The form asks for:

- **Service Name**, e.g. `checkout-api`
- **SLO Type**: Availability (requests that don't fail) or Latency (requests faster than a threshold)
- **SLO Target**, as a percentage between 90 and 99.999
- **Error Budget Window**: 7, 30 or 90 days
- **Metric Source**: which metrics your service already exports (see [Metric sources](#metric-sources) below)
- **Latency Threshold** in milliseconds, for latency SLOs only
- **SLO Name**, optional. Only needed if you want more than one SLO of the same type for a service

Hit Generate and you get:

- Prometheus recording rules: bad-event ratios over 5m, 30m, 1h, 6h, 1d and 3d, plus one over the full error budget window
- Multi-burn-rate alerting rules, following the Google SRE workbook (critical fast/slow, warning fast/slow)
- Grafana dashboard JSON you can import directly, with a bad-ratio timeseries and a gauge for the budget window

If you just want to see what comes out, pick an SLO Type and a Metric Source and hit "Try an example". It fills in a sample service for that combination and generates everything. Switch either dropdown and click it again to compare.

Once you've generated something, the page URL holds the whole form, so you can bookmark it or send it to someone. "Copy link" puts it on your clipboard.

## Metric sources

You don't need to change your instrumentation. Use the Metric Source dropdown to tell Ithildin which metrics your service already exports, and the rules are written against those. If you're not sure, look at what your `/metrics` endpoint exposes and find the matching row.

| Metric Source option | API value | Requests (availability) | Errors (availability) | Histogram (latency) |
|---|---|---|---|---|
| `<service>_requests_total` (default) | `service-counter` | `<service>_requests_total` | `status=~"5.."` | `<service>_request_duration_seconds` |
| Prometheus HTTP | `prometheus-http` | `http_requests_total{job="<service>"}` | `code=~"5.."` | `http_request_duration_seconds` |
| gRPC | `grpc` | `grpc_server_handled_total{job="<service>"}` | server-side codes (Internal, Unavailable, DeadlineExceeded, etc.) | `grpc_server_handling_seconds` |
| Istio | `istio` | `istio_requests_total{reporter="destination",destination_service_name="<service>"}` | `response_code=~"5.."` | `istio_request_duration_milliseconds` |
| ingress-nginx | `ingress-nginx` | `nginx_ingress_controller_requests{service="<service>"}` | `status=~"5.."` | `nginx_ingress_controller_request_duration_seconds` |
| Custom selectors | `custom` | whatever you enter | whatever you enter | whatever you enter |

`<service>` is whatever you typed in Service Name. For the Prometheus HTTP and gRPC options, that has to match the `job` label Prometheus scrapes your service under. Hyphens and dots in the service name are replaced with `_` in metric names, so `payment-api` becomes `payment_api_requests_total`.

If none of these fit, choose Custom selectors. For an availability SLO you get two fields, one for error requests and one for total requests, e.g. `http_server_requests_seconds_count{app="checkout",outcome="SERVER_ERROR"}`. For a latency SLO you get one field for the histogram's `_bucket` series, e.g. `http_server_requests_seconds_bucket{app="checkout"}`. Selectors are plain PromQL vector selectors: a metric name and label matchers, no ranges or functions.

## Latency SLOs

A latency SLO like "99% of requests under 300ms" is worked out from a histogram. Requests in the bucket at your threshold are good, the histogram's `_count` is the total, and the difference is bad. Ithildin picks the bucket and the `_count` series for you.

A few things to know:

- The threshold has to be one of your histogram's bucket boundaries. If your buckets are 0.1, 0.25, 0.5 and 1 seconds, 250ms works and 300ms matches nothing.
- Exporters write whole-second buckets as `le="1"`, but Prometheus 3 stores them as `le="1.0"`. The generated rules match both.
- Istio's histogram is in milliseconds, the others are in seconds. Ithildin converts for you, so always enter the threshold in ms.
- gRPC handling-time histograms are off by default in most gRPC Prometheus libraries, so turn them on in your server first.

## More than one SLO per service

Every recorded series and alert has a `service` label and an `slo` label. The `slo` label is the SLO Name, which defaults to the SLO type. So an availability SLO and a latency SLO for the same service don't clash out of the box. To have two latency SLOs (say 300ms for checkout and 1s for search), give them different names like `fast-checkout` and `search`.

Rule groups, alert names and dashboard UIDs all include the name too, so you can load as many as you like into the same Prometheus and Grafana.

## How the rules work

For each SLO you get these recording rules, all labelled with `service` and `slo`:

- `slo:sli_bad:rate5m` and `slo:sli_total:rate5m`: the per-second rate of bad and total events
- `slo:sli_error:ratio_rate5m`, `...ratio_rate30m`, and so on up to `...ratio_rate3d`: the bad ratio over each window the alerts use
- `slo:sli_error:ratio_rate30d` (or `7d`/`90d`): the bad ratio over the whole budget window

The budget window doesn't scan weeks of raw samples. It adds up the 5m rates recorded above (`sum_over_time` of bad divided by `sum_over_time` of total), which is much cheaper. Summing bad and total separately also keeps it weighted by traffic, so a quiet night doesn't count as much as a busy afternoon.

Two consequences:

- Prometheus has to keep data for at least the length of the window.
- The window rule can only see time since the recording rules were loaded. For the first 30 days it reports on however much history it has.

The rules are tested with `promtool test rules` against synthetic traffic, for both an availability and a latency SLO (see [Tests](#tests)).

If you used an earlier version of Ithildin, the rule names have changed (they used to be `job:slo_errors:rate*`). Regenerate both the rules and the dashboard.

## Grafana dashboard

The dashboard JSON is schema version 39, so it imports into Grafana 10.4 without any migration. I've tested it on 10.4.

The UID is based on the service and SLO name (`checkout-api` with the default availability SLO gets `slo-checkout_api-availability`). If you regenerate and import again, it replaces the existing dashboard rather than creating a copy.

There's no hardcoded datasource. You pick your Prometheus datasource from the dropdown at the top of the dashboard, so the same file works whether you import it in the UI, push it through the API, or provision it from disk.

## CLI

The same binary works from the command line, which is handy in CI. It takes the same fields as the form:

```bash
go install github.com/balakrishnan-vasudevan/ithildin@latest

ithildin generate -service checkout-api -slo 99.9 -source prometheus-http
ithildin generate -service checkout-api -type latency -threshold_ms 300 -slo 99 \
  -source prometheus-http -out rules/
```

By default it writes three files to the current directory, named after the service and SLO, e.g. `checkout-api-availability-recording.yaml`, `checkout-api-availability-alerting.yaml` and `checkout-api-availability-dashboard.json`. Use `-out <dir>` to put them somewhere else, or `-out -` to print the same JSON the API returns. Run `ithildin generate -h` for all the flags.

## API

The UI is a thin wrapper around one endpoint. `POST /generate` takes form fields (multipart or urlencoded) and returns JSON with `recording_rules`, `alerting_rules` and `grafana_dashboard`:

```bash
curl -s https://ithildin.onrender.com/generate \
  -d service=checkout-api -d slo=99.9 -d source=prometheus-http
```

| Field | Required | Values |
|---|---|---|
| `service` | yes | letters, digits, `-`, `_`, `.` (max 63 chars) |
| `slo` | yes | 90 to 99.999 |
| `type` | no | `availability` (default) or `latency` |
| `threshold_ms` | for latency | 1 to 60000 |
| `name` | no | lowercase letters, digits, `-`, `_` (max 32), defaults to the type |
| `window` | no | `7d`, `30d` (default), `90d` |
| `source` | no | one of the metric sources above, default `service-counter` |
| `error_selector`, `total_selector` | custom availability | PromQL vector selectors |
| `bucket_selector` | custom latency | histogram `_bucket` selector, without `le` |

Bad input gets a 400 with `{"error": "..."}` explaining what's wrong.

## Why it exists

Most teams write their SLOs down in a doc and then wire up Prometheus rules and Grafana dashboards by hand. Over time the two drift apart. With Ithildin the SLO definition is the source of truth and the rules and dashboard are generated from it.

## Using with terraform-k8s-argus

If you use the [Argus Terraform module](https://github.com/balakrishnan-vasudevan/terraform-k8s-argus) to manage your Kubernetes observability stack, you can feed Ithildin's output directly into Prometheus:

```hcl
module "argus" {
  source         = "github.com/balakrishnan-vasudevan/terraform-k8s-argus//aws"
  ithildin_rules = file("${path.module}/slo-rules.yaml")
  # ...
}
```

Copy the recording rules from the UI into `slo-rules.yaml` and pass it as `ithildin_rules`. Argus loads it into kube-prometheus-stack via `additionalPrometheusRulesMap`.

## Tech stack

- Go (`net/http`, `log/slog`)
- `gopkg.in/yaml.v3` for rule generation
- `golang.org/x/time/rate` for per-IP rate limiting
- Deployed on Render's free tier (~10ms cold start)

## Running locally

```bash
go run .
```

Open http://localhost:8000

## Tests

```bash
go test ./...
./testdata/promtool/run.sh
```

The second one needs Docker. It generates rules for a sample availability and latency SLO and runs `promtool test rules` on them with Prometheus 3.5, checking the recorded ratios and which alerts fire.

## Rate limiting

The `/generate` endpoint is limited to 20 requests per minute per IP.
