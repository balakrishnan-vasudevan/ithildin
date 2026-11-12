# Ithildin

> *The moon-letters reveal what is hidden*

Ithildin is an Observability as Code tool. You give it a service name and an SLO target, and it generates the Prometheus rules and Grafana dashboard for that SLO.

Live at: https://ithildin.onrender.com

## What it does

The form has four fields:

- **Service Name**, e.g. `checkout-api`
- **SLO Target**, as a percentage between 90 and 99.999
- **Error Budget Window**: 7, 30 or 90 days
- **Metric Source**: which metrics your service already exports (see [Metric sources](#metric-sources) below)

Hit Generate and you get:

- Prometheus recording rules: error rates over 5m, 30m, 1h, 6h, 1d and 3d, plus one over the full error budget window (7d, 30d or 90d)
- Multi-burn-rate alerting rules, following the Google SRE workbook (critical fast/slow, warning fast/slow)
- Grafana dashboard JSON you can import directly, with an error rate timeseries and an availability gauge for the budget window

If you just want to see what comes out, pick a Metric Source and hit "Try an example". It fills in a sample service for that source (including the two selector fields for Custom selectors) and generates everything. Switch the source and click it again to compare.

## Metric sources

You don't need to change your instrumentation. Use the Metric Source dropdown to tell Ithildin which metrics your service already exports, and the rules are written against those. If you're not sure, look at what your `/metrics` endpoint exposes and find the matching row.

| Metric Source option | API value | Total requests | Errors |
|---|---|---|---|
| `<service>_requests_total` (default) | `service-counter` | `<service>_requests_total` | `status=~"5.."` |
| Prometheus HTTP | `prometheus-http` | `http_requests_total{job="<service>"}` | `code=~"5.."` |
| gRPC | `grpc` | `grpc_server_handled_total{job="<service>"}` | server-side codes (Internal, Unavailable, DeadlineExceeded, etc.) |
| Istio | `istio` | `istio_requests_total{reporter="destination",destination_service_name="<service>"}` | `response_code=~"5.."` |
| ingress-nginx | `ingress-nginx` | `nginx_ingress_controller_requests{service="<service>"}` | `status=~"5.."` |
| Custom selectors | `custom` | whatever you enter | whatever you enter |

`<service>` is whatever you typed in Service Name. For the Prometheus HTTP and gRPC options, that has to match the `job` label Prometheus scrapes your service under.

If none of these fit, choose Custom selectors. Two more fields appear, one for error requests and one for total requests. Each takes a PromQL vector selector (a metric name and label matchers, no ranges or functions), for example `http_server_requests_seconds_count{app="checkout",outcome="SERVER_ERROR"}`. Each recording rule is then `sum(rate(errors[w])) / sum(rate(total[w]))`.

Hyphens and dots in the service name are replaced with `_` in metric names, so `payment-api` becomes `payment_api_requests_total`.

The budget window rule (e.g. `job:slo_errors:rate30d`) only works if Prometheus keeps data for at least that long. It's in a separate rule group evaluated every 5m, since it reads weeks of data.

## Grafana dashboard

The dashboard JSON is schema version 39, so it imports into Grafana 10.4 without any migration. I've tested it on 10.4.

The UID is based on the service name (`checkout-api` gets `slo-checkout_api`). If you regenerate and import again, it replaces the existing dashboard rather than creating a copy.

There's no hardcoded datasource. You pick your Prometheus datasource from the dropdown at the top of the dashboard, so the same file works whether you import it in the UI, push it through the API, or provision it from disk.

## API

The UI is a thin wrapper around one endpoint. `POST /generate` takes form fields and returns JSON with `recording_rules`, `alerting_rules` and `grafana_dashboard`:

```bash
curl -s https://ithildin.onrender.com/generate \
  -F service=checkout-api -F slo=99.9 -F window=30d -F source=prometheus-http
```

| Field | Required | Values |
|---|---|---|
| `service` | yes | letters, digits, `-`, `_`, `.` (max 63 chars) |
| `slo` | yes | 90 to 99.999 |
| `window` | no | `7d`, `30d` (default), `90d` |
| `source` | no | one of the metric sources above, default `service-counter` |
| `error_selector`, `total_selector` | only with `source=custom` | PromQL vector selectors |

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

## Rate limiting

The `/generate` endpoint is limited to 20 requests per minute per IP.
