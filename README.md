# Ithildin

> *The moon-letters reveal what is hidden*

Ithildin is an Observability as Code tool. You give it a service name and an SLO target, and it generates the Prometheus rules and Grafana dashboard for that SLO.

Live at: https://ithildin.onrender.com

## What it does

Input a service name and SLO target. Get:

- Prometheus recording rules: error rates over 5m, 30m, 1h, 6h, 1d and 3d, plus one over the full error budget window (7d, 30d or 90d)
- Multi-burn-rate alerting rules, following the Google SRE workbook (critical fast/slow, warning fast/slow)
- Grafana dashboard JSON you can import directly, with an error rate timeseries and an availability gauge for the budget window

## Metric sources

You don't need to change your instrumentation. Pick the convention your service already uses:

| Source | Total requests | Errors |
|---|---|---|
| `service-counter` (default) | `<service>_requests_total` | `status=~"5.."` |
| `prometheus-http` | `http_requests_total{job="<service>"}` | `code=~"5.."` |
| `grpc` | `grpc_server_handled_total{job="<service>"}` | server-side codes (Internal, Unavailable, DeadlineExceeded, etc.) |
| `istio` | `istio_requests_total{reporter="destination",destination_service_name="<service>"}` | `response_code=~"5.."` |
| `ingress-nginx` | `nginx_ingress_controller_requests{service="<service>"}` | `status=~"5.."` |
| `custom` | any vector selector | any vector selector |

For `custom`, enter two PromQL vector selectors (a metric name and label matchers, no ranges or functions). Each recording rule is then `sum(rate(errors[w])) / sum(rate(total[w]))`.

Hyphens and dots in the service name are replaced with `_` in metric names, so `payment-api` becomes `payment_api_requests_total`.

The budget window rule (e.g. `job:slo_errors:rate30d`) only works if Prometheus keeps data for at least that long. It's in a separate rule group evaluated every 5m, since it reads weeks of data.

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

Download the recording rules YAML from the UI and pass it as `ithildin_rules`. Argus loads it into kube-prometheus-stack via `additionalPrometheusRulesMap`.

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
