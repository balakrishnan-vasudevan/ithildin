# Ithildin

> *The moon-letters reveal what is hidden*

Ithildin is an Observability as Code tool that generates production-grade SLO artifacts from a simple service definition.

## What it does

Input a service name and SLO target. Get:

- **Prometheus recording rules** — pre-computed error rate windows (5m, 30m, 1h, 6h)
- **Multi-burn-rate alerting rules** — Google SRE Book standard (critical fast/slow, warning fast/slow)
- **Grafana dashboard JSON** — ready to import, with SLO compliance gauge and error rate timeseries

## Why it exists

Most teams define SLOs in a doc somewhere and then manually wire up Prometheus rules and Grafana dashboards. This creates drift, inconsistency, and toil. Ithildin treats your SLO definition as the source of truth and generates everything downstream from it automatically.

## Tech stack

- Python / FastAPI
- Jinja2 templates
- PyYAML for rule generation
- Deployed on Render

## Running locally

```bash
pip install -r requirements.txt
uvicorn main:app --reload
```

Open http://localhost:8000
