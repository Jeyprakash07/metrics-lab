# Metrics Lab: Prometheus + Grafana through a Go service

A toy e-commerce **order-service** in Go that exposes about 60 metric families. Five
replicas run behind a load generator, alongside a batch job, a Pushgateway,
Prometheus and Grafana, all from one `docker compose up`.

```bash
docker compose up -d --build
```

| What | URL |
|---|---|
| Grafana dashboard "Order Service" | http://localhost:3000 (no login) |
| Prometheus: targets, SD, rules, alerts | http://localhost:9090/targets · /service-discovery · /rules · /alerts |
| Pushgateway | http://localhost:9091 |
| Raw `/metrics` of one replica | `docker compose exec loadgen wget -qO- http://order-service:8080/metrics` |

```bash
./status.sh                                    # containers, targets, req/s, p99, alerts
docker compose up -d --scale order-service=5   # watch targets appear, no config change
docker compose down -v                         # stop and delete stored data
```

---

## 1. The big picture

```
                 ┌──────────── Service discovery ─────────────┐
                 │ Docker API / Kubernetes API / DNS / files  │
                 └──────────────────┬─────────────────────────┘
                                    │ "these targets exist" (+ __meta_* labels)
                                    ▼
 order-service-1 ◄──GET /metrics──┐ ┌──────────────────────────┐   PromQL over HTTP   ┌─────────┐
 order-service-2 ◄──GET /metrics──┼─┤ Prometheus               │◄────────────────────┤ Grafana │
 order-service-N ◄──GET /metrics──┘ │  scrape → relabel → TSDB │  /api/v1/query_range └─────────┘
 pushgateway     ◄──GET /metrics────┤  rules → alerts          │──► Alertmanager → Slack/PagerDuty
       ▲                            │  (local disk, 15d)       │──► remote_write → Mimir/Thanos (years)
       │ push (batch only)          └──────────────────────────┘
   batch-job
```

* **The app** (`order-service/metrics.go`) keeps counters, gauges and histograms **in memory**.
  It sends nothing anywhere.
* **Prometheus** pulls: every `scrape_interval` it sends `GET /metrics` to every target
  and stores the samples in its own time-series database (TSDB) on local disk.
* **Grafana** stores **no metrics**. It is a query and visualization front end that sends
  PromQL to Prometheus and draws what comes back. Its own state (dashboards, users) is
  config, provisioned here from `grafana/provisioning` and `grafana/dashboards`.

## 2. What `/metrics` actually looks like

```
# HELP shop_http_requests_total HTTP requests handled, by method, route pattern and status code.
# TYPE shop_http_requests_total counter
shop_http_requests_total{code="200",method="GET",route="GET /products/{sku}"} 18234
shop_http_requests_total{code="404",method="GET",route="GET /orders/{id}"} 911
# TYPE shop_http_request_duration_seconds histogram
shop_http_request_duration_seconds_bucket{method="GET",route="GET /products/{sku}",le="0.005"} 14600
shop_http_request_duration_seconds_bucket{method="GET",route="GET /products/{sku}",le="0.01"} 17010
...
shop_http_request_duration_seconds_bucket{method="GET",route="GET /products/{sku}",le="+Inf"} 18234
shop_http_request_duration_seconds_sum{...} 61.2
shop_http_request_duration_seconds_count{...} 18234
```

It is plain text holding the **current cumulative values**. Prometheus adds the timestamp
and the `job` and `instance` labels. A **time series** is one unique *metric name + label set*.
Each replica exposes about 350 series here. Across 5 replicas plus the others, Prometheus
holds about 4–5k series. Check it yourself: `count({__name__=~".+"})`.

## 3. Why pull and not push (at large scale)

| Concern | Pull (Prometheus) | Push (StatsD, many agents) |
|---|---|---|
| **Is the target alive?** | Free: the `up` series is 0 when a scrape fails. Alert `up == 0`. | Silence looks the same as "no traffic". You need a separate heartbeat. |
| **Who controls load?** | The monitoring system sets the rate (`scrape_interval`). A buggy app can't flood it. | Every client decides. One bad deploy pushing in a tight loop can DoS the collector. |
| **What should be monitored?** | Service discovery is the source of truth: the platform says what *should* exist. | You only know about whatever happened to call in. |
| **Debugging** | `curl pod:8080/metrics` shows exactly what Prometheus sees. | You need the pipeline to see anything. |
| **App coupling** | The app doesn't know where monitoring lives. No config, no credentials. | Every app needs the collector's address and auth. |
| **HA** | Run two identical Prometheus servers scraping the same targets. Nothing changes in the apps. | Clients must fan out or go through a load balancer. |

Push still has its place:
* **Short-lived batch jobs** → Pushgateway (`order-service/batch.go`). The job may live
  3 s, so a 15 s scrape would miss it. It pushes its result, and Prometheus scrapes the
  gateway with `honor_labels: true`. Alert on `time() - shop_batch_last_success_timestamp_seconds`.
* **Targets you can't reach** (behind NAT, edge devices) → push via OpenTelemetry/OTLP, or
  a local agent that scrapes and `remote_write`s outward.
* **Prometheus → long-term storage** uses `remote_write`, which is push between
  *monitoring systems*, not from apps.

At large scale the usual layout is: **one Prometheus (or a sharded, HA pair) per cluster
pulls locally**, then `remote_write`s to a central **Mimir / Thanos / Cortex /
VictoriaMetrics** that Grafana queries for the global view. Scraping stays local and
cheap; only aggregated or complete data travels.

## 4. Service discovery, the core of it

Nobody lists 3,000 pod IPs by hand. Prometheus asks a **discovery source** what exists,
gets targets with `__meta_*` labels, and **relabeling** turns those into scrape addresses
and clean labels. Open http://localhost:9090/service-discovery to see the "Discovered
labels" (before) and "Target labels" (after) side by side.

This lab runs four mechanisms (`prometheus/prometheus.yml`):

| Job | Mechanism | Production equivalent |
|---|---|---|
| `prometheus` | `static_configs` | Prometheus scraping itself |
| `order-service` | `docker_sd_configs` + label opt-in (`prometheus.scrape=true`) | **`kubernetes_sd_configs` role pod + `prometheus.io/scrape` annotations**, or a **ServiceMonitor / PodMonitor** with the Prometheus Operator |
| `grafana` | `dns_sd_configs` (A record → all IPs) | Kubernetes headless Service, Consul DNS |
| `pushgateway` | `file_sd_configs` (`targets/*.json`, hot-reloaded) | Anything that can write a file: Ansible, CMDB sync, custom scripts |

Key relabeling moves in the `order-service` job:
1. `__meta_docker_network_ip` + label `prometheus.port` → `__address__` (where to scrape)
2. label `prometheus.path` → `__metrics_path__`
3. container name → `instance` (stable and readable, instead of an IP that changes on restart)
4. compose service → `service`. Every `__*` label is dropped after relabeling.

**Try it:** `docker compose up -d --scale order-service=8`. About 10 s later the new
targets appear in `/targets` and in the dashboard's *Instance* dropdown.

## 5. Retention and storage

Set with **flags**, not in `prometheus.yml` (see `docker-compose.yml`):

```
--storage.tsdb.retention.time=15d   # delete data older than 15 days (default 15d)
--storage.tsdb.retention.size=5GB   # ...or when the TSDB exceeds 5GB, whichever comes first
```

How the TSDB works on disk (`docker compose exec prometheus ls /prometheus`):
* New samples go to an in-memory **head block**, protected by a **WAL** (write-ahead log)
  so a crash loses nothing.
* Every 2 h the head is flushed into an immutable **block** directory (chunks + index).
  Blocks are **compacted** into larger ones (up to 10% of retention, max 31 days).
* Retention deletes **whole blocks**, so data may stay a bit past the limit.
* Rough sizing: **about 1–2 bytes per sample** after compression.
  `disk ≈ series × (86400 / scrape_interval) × days × 1.5 B`.
  Example: 1M series at a 15 s scrape for 15 days ≈ 1M × 5760 × 15 × 1.5 B ≈ **130 GB**.
* Memory scales with **active series** (about 3–4 KB each in the head). **Cardinality** is
  what kills Prometheus, not sample rate. That's why `route` uses the pattern
  `GET /orders/{id}` and never the raw path (`server.go`, the `instrument` function).

Local Prometheus is **not** meant for long-term or HA-durable storage. For months or years:
`remote_write` to Mimir/Cortex/Thanos Receive/VictoriaMetrics, or Thanos Sidecar
(uploads blocks to S3). Downsampling and long retention happen there. Recording rules
(`prometheus/rules/recording.yml`) precompute expensive aggregates, so dashboards and
long-term storage use small series instead of raw per-pod data.

Useful self-metrics: `prometheus_tsdb_head_series`, `prometheus_tsdb_storage_blocks_bytes`,
`rate(prometheus_tsdb_head_samples_appended_total[5m])`, `scrape_duration_seconds`,
`scrape_samples_scraped`.

## 6. Grafana setup

* **Data source** provisioned in `grafana/provisioning/datasources/prometheus.yml`
  (`access: proxy`: the Grafana server queries Prometheus, not your browser).
* **Dashboards** provisioned from JSON in `grafana/dashboards/`. That JSON is generated by
  `grafana/gen_dashboard.py` (dashboards-as-code: review in PRs, identical in every
  environment). Re-run the script and Grafana reloads the file within 30 s.
* **Variables**: `$instance` comes from `label_values(up{job="order-service"}, instance)`,
  so the dropdown is driven by service discovery too.
* **`$__rate_interval`**: use it in every `rate()`. Grafana picks a window of at least
  4× the scrape interval, so graphs don't go blank when you zoom in.
* **Annotations**: a red marker appears when chaos is active (query-based annotation).

## 7. Things to do (exercises)

1. **Cause an incident.** For each replica:
   ```bash
   for ip in $(docker compose ps -q order-service | xargs docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'); do docker compose exec -T loadgen wget -qO- --post-data= "http://$ip:8080/admin/chaos?latency_ms=300&error_rate=0.2"; done
   ```
   Watch the error ratio, p99 and heatmap change. Open `/alerts`: `HighErrorRate` goes
   *pending*, then *firing* after 2 min. Reset with `latency_ms=0&error_rate=0`.
   Note that chaos is **per process**, so if you hit only one replica, filter
   `$instance` to find it.
2. **Kill a target.** `docker stop metrics-lab-order-service-2`, then look at `up`, the
   *Targets up* stat, and the `TargetDown` alert. This is the free signal pull gives you.
3. **Summary vs histogram.** Compare the *Cart items (Summary)* panel (one line per
   instance and quantile, impossible to combine correctly) with *Order value
   (Histogram)* (one correct global p90).
4. **Mean vs p99.** In *Latency percentiles*, the mean stays low while p99 shows the 1%
   long-tail payment calls (`server.go`, `charge`).
5. **Make a cardinality bomb (then undo it).** In `instrument`, change `route := r.Pattern`
   to `route := r.URL.Path`, rebuild, and watch `count({__name__="shop_http_requests_total"})`
   climb as order ids become label values.
6. **Saturate the queue.** `docker compose up -d --scale order-service=1` and raise the
   loadgen `-rps` to 150. The queue fills, 503s appear with `reason="queue_full"`, and
   `QueueSaturated` fires.
7. **Use the Prometheus UI.** Type a raw counter `shop_http_requests_total` (it only
   grows, which is useless to graph), then `rate(shop_http_requests_total[1m])` (useful).

## 8. Where the code lives

| File | Teaches |
|---|---|
| `order-service/metrics.go` | Every metric type, naming, labels, GaugeFunc, info metric, custom registry |
| `order-service/server.go` | HTTP middleware (RED), the `/metrics` handler, low-cardinality route labels, worker pipeline (USE) |
| `order-service/batch.go` | Pushgateway: the push exception |
| `order-service/loadgen.go` | Realistic traffic with a 10-min wave |
| `prometheus/prometheus.yml` | Scraping, 4 SD types, relabeling, external_labels, remote_write |
| `prometheus/rules/*.yml` | Recording rules (naming `level:metric:ops`) and alerts |
| `grafana/…` | Provisioning, dashboard-as-code |

**Next lesson:** metric types in depth (counter resets, `rate` vs `irate` vs `increase`,
histogram bucket design, native histograms, exemplars) and choosing visualizations
(time series vs stat vs heatmap vs bar gauge, units, thresholds, SLO burn-rate panels).
