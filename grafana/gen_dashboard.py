"""Generates grafana/dashboards/order-service.json.
Dashboards are just JSON; generating them from code (grafonnet, grafana-foundation-sdk,
or a script like this) keeps them reviewable in git."""
import json, os

DS = {"type": "prometheus", "uid": "prometheus"}
SEL = 'job="order-service", instance=~"$instance"'
panels, y = [], 0
pid = 0

def row(title):
    global y, pid
    pid += 1
    panels.append({"id": pid, "type": "row", "title": title, "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": []})
    y += 1

def panel(title, exprs, x, w, h=8, kind="timeseries", unit="short", desc="", stack=False,
          extra=None, fmt=None):
    global pid
    pid += 1
    targets = []
    for i, e in enumerate(exprs):
        expr, legend = e if isinstance(e, tuple) else (e, "")
        t = {"refId": chr(65 + i), "datasource": DS, "expr": expr, "legendFormat": legend or "__auto"}
        if fmt: t["format"] = fmt
        if kind == "stat": t["instant"] = False
        targets.append(t)
    p = {"id": pid, "type": kind, "title": title, "description": desc, "datasource": DS,
         "gridPos": {"h": h, "w": w, "x": x, "y": y}, "targets": targets,
         "fieldConfig": {"defaults": {"unit": unit, "custom": {}}, "overrides": []},
         "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
                     "tooltip": {"mode": "multi", "sort": "desc"}}}
    if kind == "timeseries":
        p["fieldConfig"]["defaults"]["custom"] = {"lineWidth": 1, "fillOpacity": 25 if stack else 8,
            "showPoints": "never", "stacking": {"mode": "normal" if stack else "none"}}
    if kind == "stat":
        p["fieldConfig"]["defaults"]["thresholds"] = {"mode": "absolute", "steps": [{"color": "green", "value": None}]}
        p["options"] = {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value",
                        "graphMode": "area", "textMode": "auto"}
    if extra: deep(p, extra)
    panels.append(p)

def deep(a, b):
    for k, v in b.items():
        if isinstance(v, dict) and isinstance(a.get(k), dict): deep(a[k], v)
        else: a[k] = v

def thresholds(*steps):
    return {"fieldConfig": {"defaults": {"thresholds": {"mode": "absolute",
            "steps": [{"color": c, "value": v} for v, c in steps]}}}}

R = "$__rate_interval"

# ---------------- Overview ----------------
row("Overview — golden signals")
panel("Targets up", [f'sum(up{{{SEL}}})'], 0, 4, 5, "stat",
      desc="`up` is written by Prometheus itself after every scrape: 1 = scrape OK.",
      extra=thresholds((None, "red"), (1, "orange"), (3, "green")))
panel("Request rate", [f'sum(rate(shop_http_requests_total{{{SEL}}}[{R}]))'], 4, 4, 5, "stat", "reqps",
      desc="COUNTER -> rate(): per-second increase, averaged over the window, reset-safe.")
panel("Error ratio (5xx)", [f'sum(rate(shop_http_requests_total{{{SEL},code=~"5.."}}[{R}])) / sum(rate(shop_http_requests_total{{{SEL}}}[{R}]))'],
      8, 4, 5, "stat", "percentunit", extra=thresholds((None, "green"), (0.01, "orange"), (0.05, "red")))
panel("p99 latency", [f'histogram_quantile(0.99, sum by (le) (rate(shop_http_request_duration_seconds_bucket{{{SEL}}}[{R}])))'],
      12, 4, 5, "stat", "s", desc="HISTOGRAM: sum buckets by le across instances, then estimate the quantile.",
      extra=thresholds((None, "green"), (0.25, "orange"), (0.5, "red")))
panel("Queue depth", [f'sum(shop_queue_depth{{{SEL}}})'], 16, 4, 5, "stat",
      desc="GAUGE: read the current value directly. Never rate() a gauge.")
panel("Orders / min", [f'sum(rate(shop_orders_created_total{{{SEL}}}[{R}])) * 60'], 20, 4, 5, "stat", "short")
y += 5

# ---------------- HTTP ----------------
row("HTTP (RED: Rate, Errors, Duration)")
panel("Requests/s by route", [(f'sum by (route) (rate(shop_http_requests_total{{{SEL}}}[{R}]))', "{{route}}")], 0, 12, unit="reqps", stack=True)
panel("Non-2xx responses/s by code", [(f'sum by (code) (rate(shop_http_requests_total{{{SEL},code!~"2.."}}[{R}]))', "{{code}}")], 12, 12, unit="reqps", stack=True)
y += 8
panel("Latency percentiles (all routes)", [
    (f'histogram_quantile(0.50, sum by (le) (rate(shop_http_request_duration_seconds_bucket{{{SEL}}}[{R}])))', "p50"),
    (f'histogram_quantile(0.95, sum by (le) (rate(shop_http_request_duration_seconds_bucket{{{SEL}}}[{R}])))', "p95"),
    (f'histogram_quantile(0.99, sum by (le) (rate(shop_http_request_duration_seconds_bucket{{{SEL}}}[{R}])))', "p99"),
    (f'sum(rate(shop_http_request_duration_seconds_sum{{{SEL}}}[{R}])) / sum(rate(shop_http_request_duration_seconds_count{{{SEL}}}[{R}]))', "mean (_sum/_count)"),
], 0, 12, unit="s", desc="Mean hides the tail. Compare it with p99.")
panel("Latency heatmap", [(f'sum by (le) (increase(shop_http_request_duration_seconds_bucket{{{SEL}}}[{R}]))', "{{le}}")],
      12, 12, kind="heatmap", unit="s", fmt="heatmap",
      desc="Each column = one time step, each cell = how many requests landed in that bucket.",
      extra={"options": {"calculate": False, "yAxis": {"unit": "s"}, "color": {"scheme": "Oranges", "mode": "scheme"},
                         "cellGap": 1, "tooltip": {"show": True, "yHistogram": True}}})
y += 8
panel("p95 latency by route", [(f'histogram_quantile(0.95, sum by (route, le) (rate(shop_http_request_duration_seconds_bucket{{{SEL}}}[{R}])))', "{{route}}")], 0, 12, unit="s")
panel("In-flight requests by instance", [(f'shop_http_requests_in_flight{{{SEL}}}', "{{instance}}")], 12, 12)
y += 8

# ---------------- Business ----------------
row("Business")
panel("Orders/s by region", [(f'sum by (region) (rate(shop_orders_created_total{{{SEL}}}[{R}]))', "{{region}}")], 0, 8, unit="short", stack=True)
panel("Failed orders/s by reason", [(f'sum by (reason) (rate(shop_orders_failed_total{{{SEL}}}[{R}]))', "{{reason}}")], 8, 8, stack=True)
panel("Revenue/min (approx from histogram _sum)", [(f'sum by (region) (rate(shop_orders_value_dollars_sum{{{SEL}}}[{R}])) * 60', "{{region}}")], 16, 8, unit="currencyUSD", stack=True)
y += 8
panel("Cart items quantiles (SUMMARY, per instance)", [(f'shop_orders_cart_items{{{SEL}}}', "{{instance}} q{{quantile}}")], 0, 12,
      desc="Summary quantiles are precomputed per process. You can NOT average them across instances. Use a histogram for that.")
panel("Order value p50 / p90 (HISTOGRAM, all instances)", [
    (f'histogram_quantile(0.5, sum by (le) (rate(shop_orders_value_dollars_bucket{{{SEL}}}[{R}])))', "p50"),
    (f'histogram_quantile(0.9, sum by (le) (rate(shop_orders_value_dollars_bucket{{{SEL}}}[{R}])))', "p90")], 12, 12, unit="currencyUSD")
y += 8

# ---------------- Pipeline ----------------
row("Async pipeline (USE: Utilization, Saturation, Errors)")
panel("Queue depth vs capacity", [(f'sum(shop_queue_depth{{{SEL}}})', "depth"), (f'sum(shop_queue_capacity{{{SEL}}})', "capacity")], 0, 8)
panel("Worker utilization", [(f'sum by (instance) (shop_workers_busy{{{SEL}}}) / sum by (instance) (shop_workers_total{{{SEL}}})', "{{instance}}")], 8, 8, unit="percentunit",
      extra={"fieldConfig": {"defaults": {"max": 1, "min": 0}}})
panel("Stage p95 duration", [(f'histogram_quantile(0.95, sum by (stage, le) (rate(shop_pipeline_stage_duration_seconds_bucket{{{SEL}}}[{R}])))', "{{stage}}")], 16, 8, unit="s")
y += 8
panel("Payment outcomes/s", [(f'sum by (provider, outcome) (rate(shop_payment_requests_total{{{SEL}}}[{R}]))', "{{provider}} {{outcome}}")], 0, 8, stack=True)
panel("Payment p99 by provider", [(f'histogram_quantile(0.99, sum by (provider, le) (rate(shop_payment_latency_seconds_bucket{{{SEL}}}[{R}])))', "{{provider}}")], 8, 8, unit="s")
panel("Cache hit ratio", [(f'sum(rate(shop_cache_requests_total{{{SEL},result="hit"}}[{R}])) / sum(rate(shop_cache_requests_total{{{SEL}}}[{R}]))', "hit ratio")], 16, 8, unit="percentunit")
y += 8
panel("Lowest inventory (bottomk)", [(f'bottomk(5, avg by (sku) (shop_inventory_stock_level{{{SEL}}}))', "{{sku}}")], 0, 12, kind="bargauge",
      desc="Instant query: bottomk over a range would pick different SKUs at each step.",
      extra={"targets": [{"refId": "A", "datasource": DS, "instant": True, "range": False, "legendFormat": "{{sku}}",
                          "expr": f'bottomk(5, avg by (sku) (shop_inventory_stock_level{{{SEL}}}))'}],
             "fieldConfig": {"defaults": {"min": 0, "max": 900}}, "options": {"orientation": "horizontal", "displayMode": "gradient", "reduceOptions": {"calcs": ["lastNotNull"]}}})
panel("Chaos injected", [(f'max(shop_chaos_injected_latency_seconds{{{SEL}}})', "latency (s)"), (f'max(shop_chaos_injected_error_ratio{{{SEL}}})', "error ratio")], 12, 12)
y += 8

# ---------------- Runtime ----------------
row("Go runtime & process (free from client_golang collectors)")
panel("Goroutines", [(f'go_goroutines{{{SEL}}}', "{{instance}}")], 0, 6)
panel("Heap in use", [(f'go_memstats_heap_inuse_bytes{{{SEL}}}', "{{instance}}")], 6, 6, unit="bytes")
panel("CPU cores used", [(f'rate(process_cpu_seconds_total{{{SEL}}}[{R}])', "{{instance}}")], 12, 6)
panel("Version per instance (info metric)", [(f'shop_build_info{{{SEL}}}', "{{instance}} v{{version}} {{goversion}}")], 18, 6, kind="table", fmt="table",
      extra={"transformations": [{"id": "organize", "options": {"excludeByName": {"Time": True, "Value": True, "__name__": True, "job": True, "env": True, "service": True}}}]})
y += 8

# ---------------- Batch ----------------
row("Batch job (pushed via Pushgateway)")
panel("Seconds since last successful run", [('time() - shop_batch_last_success_timestamp_seconds', "{{job}}")], 0, 8, unit="s",
      extra=thresholds((None, "green"), (120, "orange"), (300, "red")))
panel("Records processed (last run)", [('shop_batch_records_processed', "{{job}}")], 8, 8)
panel("Scrape duration per target (Prometheus self-metric)", [('scrape_duration_seconds', "{{job}} {{instance}}")], 16, 8, unit="s")
y += 8

dash = {
    "uid": "order-service", "title": "Order Service", "tags": ["metrics-lab"],
    "timezone": "browser", "schemaVersion": 39, "version": 1, "editable": True,
    "refresh": "10s", "time": {"from": "now-30m", "to": "now"},
    "templating": {"list": [{
        "name": "instance", "label": "Instance", "type": "query", "datasource": DS,
        "query": {"query": 'label_values(up{job="order-service"}, instance)', "refId": "A"},
        "definition": 'label_values(up{job="order-service"}, instance)',
        "includeAll": True, "multi": True, "allValue": ".*",
        "current": {"text": "All", "value": "$__all"}, "refresh": 2, "sort": 1}]},
    "annotations": {"list": [{"name": "Chaos on", "datasource": DS, "enable": True, "iconColor": "red",
        "expr": "max(shop_chaos_injected_error_ratio) > 0 or max(shop_chaos_injected_latency_seconds) > 0",
        "step": "30s", "titleFormat": "chaos active"}]},
    "panels": panels,
}
out = os.path.join(os.path.dirname(__file__), "dashboards", "order-service.json")
json.dump(dash, open(out, "w"), indent=2)
print("wrote", out, len(panels), "panels")
