#!/usr/bin/env bash
# Quick health check of the metrics lab: containers, scrape targets, key numbers, alerts.
# Usage: ./status.sh        (override Prometheus URL with PROM=http://host:9090)
cd "$(dirname "$0")"
PROM=${PROM:-http://localhost:9090}

echo "== Containers"
if ! docker compose ps --format '   {{.Name}}\t{{.Status}}' 2>/dev/null; then
  echo "   Docker is not reachable. Start Docker, then: docker compose up -d"
fi

if ! curl -sf "$PROM/-/ready" >/dev/null; then
  echo "== Prometheus at $PROM is not responding. Start the stack: docker compose up -d"
  exit 1
fi

q() {
  curl -s "$PROM/api/v1/query" --data-urlencode "query=$1" | python3 -c '
import json, sys
res = json.load(sys.stdin)["data"]["result"]
if not res:
    print("    (no data yet)")
for r in res:
    m = dict((k, v) for k, v in r["metric"].items() if k not in ("__name__", "env", "service"))
    labels = ", ".join("%s=%s" % kv for kv in m.items()) or "(total)"
    print("    %s => %s" % (labels, round(float(r["value"][1]), 4)))'
}

echo "== Scrape targets (Prometheus pulls each one; up=1 healthy)"
curl -s "$PROM/api/v1/targets" | python3 -c '
import json, sys
for t in json.load(sys.stdin)["data"]["activeTargets"]:
    l = t["labels"]
    print("    %-7s %-14s %-30s %s" % (t["health"], l.get("job"), l.get("instance"), t.get("lastError", "")))'

echo "== Requests/s";            q 'sum(rate(shop_http_requests_total[1m]))'
echo "== 5xx error ratio";       q 'job:shop_http_errors:ratio_rate5m'
echo "== p99 latency (s)";       q 'histogram_quantile(0.99, sum by (le) (rate(shop_http_request_duration_seconds_bucket[1m])))'
echo "== Queue depth";           q 'sum(shop_queue_depth)'
echo "== Active series in TSDB"; q 'prometheus_tsdb_head_series'

echo "== Alerts"
curl -s "$PROM/api/v1/alerts" | python3 -c '
import json, sys
alerts = json.load(sys.stdin)["data"]["alerts"]
if not alerts:
    print("    none")
for a in alerts:
    print("    %-8s %s %s" % (a["state"], a["labels"]["alertname"], a["labels"].get("instance", "")))'
