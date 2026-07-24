# Ithildin

> *The moon-letters reveal what is hidden*

Ithildin is an Observability as Code tool that generates production-grade SLO artifacts from a simple service definition.

Live at: https://ithildin.onrender.com

## What it does

Input a service name and SLO target. Get:

- **Prometheus recording rules** — pre-computed error rate windows (5m, 30m, 1h, 6h)
- **Multi-burn-rate alerting rules** — Google SRE Book standard (critical fast/slow, warning fast/slow)
- **Grafana dashboard JSON** — ready to import, with SLO compliance gauge and error rate timeseries

## Why it exists

Most teams define SLOs in a doc somewhere and then manually wire up Prometheus rules and Grafana dashboards. This creates drift, inconsistency, and toil. Ithildin treats your SLO definition as the source of truth and generates everything downstream from it automatically.

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
- Deployed on Render (free tier — ~10ms cold start)

## Running locally

```bash
go run .
```

Open http://localhost:8000

## Rate limiting

The `/generate` endpoint is limited to 20 requests per minute per IP.
