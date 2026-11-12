#!/bin/sh
# Generates rules for two sample SLOs and runs promtool's rule unit tests
# against them, using the Prometheus Docker image.
#   ./testdata/promtool/run.sh
set -e
cd "$(dirname "$0")"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
(cd ../.. && go run . generate -service checkout-api -slo 99.9 -window 7d -source prometheus-http -out "$out" >/dev/null)
(cd ../.. && go run . generate -service checkout-api -slo 99 -type latency -threshold_ms 1000 -source prometheus-http -out "$out" >/dev/null)
cp ./*_test.yaml "$out"
docker run --rm -v "$out":/r -w /r --entrypoint promtool prom/prometheus:v3.5.0 \
  test rules avail_test.yaml latency_test.yaml
