from fastapi import FastAPI, Request, Form
from fastapi.responses import HTMLResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates
import uvicorn
import json

app = FastAPI(title="Ithildin - Observability as Code")
templates = Jinja2Templates(directory="templates")

def generate_recording_rules(service: str, slo: float, window: str = "30d") -> dict:
    error_rate = round(1 - slo / 100, 6)
    return {
        "groups": [
            {
                "name": f"{service}.slo.rules",
                "interval": "30s",
                "rules": [
                    {
                        "record": f"job:slo_errors:rate5m",
                        "expr": f'sum(rate({service}_requests_total{{status=~"5.."}}[5m])) / sum(rate({service}_requests_total[5m]))',
                        "labels": {"service": service}
                    },
                    {
                        "record": f"job:slo_errors:rate30m",
                        "expr": f'sum(rate({service}_requests_total{{status=~"5.."}}[30m])) / sum(rate({service}_requests_total[30m]))',
                        "labels": {"service": service}
                    },
                    {
                        "record": f"job:slo_errors:rate1h",
                        "expr": f'sum(rate({service}_requests_total{{status=~"5.."}}[1h])) / sum(rate({service}_requests_total[1h]))',
                        "labels": {"service": service}
                    },
                    {
                        "record": f"job:slo_errors:rate6h",
                        "expr": f'sum(rate({service}_requests_total{{status=~"5.."}}[6h])) / sum(rate({service}_requests_total[6h]))',
                        "labels": {"service": service}
                    },
                ]
            }
        ]
    }

def generate_alerting_rules(service: str, slo: float, window: str = "30d") -> dict:
    error_rate = round(1 - slo / 100, 6)
    # Google multi-burn-rate thresholds
    burn_rates = [
        {"name": "CriticalFast", "severity": "critical", "short": "5m", "long": "1h", "burn": 14.4, "for": "2m"},
        {"name": "CriticalSlow", "severity": "critical", "short": "30m", "long": "6h", "burn": 6, "for": "15m"},
        {"name": "WarningFast", "severity": "warning", "short": "1h", "long": "1d", "burn": 3, "for": "1h"},
        {"name": "WarningSlow", "severity": "warning", "short": "6h", "long": "3d", "burn": 1, "for": "3h"},
    ]

    rules = []
    for br in burn_rates:
        threshold = round(error_rate * br["burn"], 6)
        rules.append({
            "alert": f"{service}ErrorBudget{br['name']}",
            "expr": (
                f'job:slo_errors:rate{br["short"]}{{service="{service}"}} > {threshold}\n'
                f'and\n'
                f'job:slo_errors:rate{br["long"]}{{service="{service}"}} > {threshold}'
            ),
            "for": br["for"],
            "labels": {
                "severity": br["severity"],
                "service": service
            },
            "annotations": {
                "summary": f'{service} error budget burn rate too high ({br["name"]})',
                "description": (
                    f'Error budget for {service} (SLO: {slo}%) is being consumed at '
                    f'{br["burn"]}x the acceptable rate. '
                    f'Short window ({br["short"]}): {{{{ $value | humanizePercentage }}}}.'
                )
            }
        })

    return {
        "groups": [
            {
                "name": f"{service}.slo.alerts",
                "rules": rules
            }
        ]
    }

def generate_grafana_dashboard(service: str, slo: float) -> dict:
    return {
        "title": f"{service} SLO Dashboard",
        "tags": ["slo", "observability", "ithildin"],
        "panels": [
            {
                "title": "Error Rate",
                "type": "timeseries",
                "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0},
                "targets": [
                    {
                        "expr": f'job:slo_errors:rate5m{{service="{service}"}}',
                        "legendFormat": "5m error rate"
                    }
                ]
            },
            {
                "title": f"SLO Compliance ({slo}%)",
                "type": "gauge",
                "gridPos": {"h": 8, "w": 12, "x": 12, "y": 0},
                "targets": [
                    {
                        "expr": f'1 - job:slo_errors:rate{"{"}service="{service}"{"}"}',
                        "legendFormat": "availability"
                    }
                ],
                "fieldConfig": {
                    "defaults": {
                        "thresholds": {
                            "steps": [
                                {"color": "red", "value": 0},
                                {"color": "yellow", "value": slo / 100 - 0.001},
                                {"color": "green", "value": slo / 100}
                            ]
                        }
                    }
                }
            }
        ]
    }

import yaml

def dict_to_yaml(d):
    return yaml.dump(d, default_flow_style=False, allow_unicode=True, sort_keys=False)

@app.get("/", response_class=HTMLResponse)
async def index(request: Request):
    return templates.TemplateResponse("index.html", {"request": request})

@app.post("/generate", response_class=JSONResponse)
async def generate(
    service: str = Form(...),
    slo: float = Form(...),
    window: str = Form("30d")
):
    recording = generate_recording_rules(service, slo, window)
    alerting = generate_alerting_rules(service, slo, window)
    dashboard = generate_grafana_dashboard(service, slo)

    return {
        "recording_rules": dict_to_yaml(recording),
        "alerting_rules": dict_to_yaml(alerting),
        "grafana_dashboard": json.dumps(dashboard, indent=2)
    }

if __name__ == "__main__":
    uvicorn.run("main:app", host="0.0.0.0", port=8000, reload=True)
