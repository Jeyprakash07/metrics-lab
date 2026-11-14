package main

// metrics.go — every metric this service exposes, in one place.
//
// LESSON: a metric is just a named number that lives in this process's memory.
// Nothing here sends data anywhere. When Prometheus scrapes GET /metrics, the
// client library walks the registry and prints every current value as text.
// That is the whole "pull" model: the app keeps counters, Prometheus comes and reads them.
//
// Naming conventions (https://prometheus.io/docs/practices/naming/):
//   - <namespace>_<subsystem>_<name>_<unit>
//   - base units only: seconds, bytes (never milliseconds or megabytes)
//   - counters end in _total
//   - labels hold dimensions with FEW possible values (route, code, region),
//     never unbounded ones (user_id, order_id, full URL), because every unique
//     combination of label values creates a new time series in Prometheus.

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "shop"

type Metrics struct {
	Registry *prometheus.Registry

	// ---- HTTP layer: the "RED" method (Rate, Errors, Duration) ----
	HTTPRequests *prometheus.CounterVec   // COUNTER: only goes up. Use rate() on it.
	HTTPDuration *prometheus.HistogramVec // HISTOGRAM: counts observations per bucket -> percentiles
	HTTPInFlight prometheus.Gauge         // GAUGE: goes up and down. Read it directly.
	HTTPRespSize *prometheus.HistogramVec

	// ---- Business metrics: what the service is actually for ----
	OrdersCreated *prometheus.CounterVec
	OrdersFailed  *prometheus.CounterVec
	OrderValue    *prometheus.HistogramVec
	CartItems     prometheus.Summary // SUMMARY: quantiles computed in the app (cannot be aggregated!)

	// ---- Async pipeline: the "USE" method (Utilization, Saturation, Errors) ----
	WorkersBusy     prometheus.Gauge
	StageDuration   *prometheus.HistogramVec
	OrdersProcessed *prometheus.CounterVec

	// ---- Dependencies ----
	PaymentRequests *prometheus.CounterVec
	PaymentLatency  *prometheus.HistogramVec
	CacheRequests   *prometheus.CounterVec
	DBQueryDuration *prometheus.HistogramVec

	// ---- State ----
	InventoryLevel *prometheus.GaugeVec

	// ---- Chaos knobs (so you can see incidents on the dashboard) ----
	ChaosLatency   prometheus.Gauge
	ChaosErrorRate prometheus.Gauge
}

func NewMetrics(version string, queueLen, queueCap func() int, workers int) *Metrics {
	// A dedicated registry instead of the global default one: explicit, testable,
	// and you control exactly what is exported.
	reg := prometheus.NewRegistry()

	// Free metrics: Go runtime (goroutines, GC, heap) and process (CPU, RSS, open fds).
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	f := promauto.With(reg) // promauto = create + register in one step
	m := &Metrics{Registry: reg}

	m.HTTPRequests = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http", Name: "requests_total",
		Help: "HTTP requests handled, by method, route pattern and status code.",
	}, []string{"method", "route", "code"})

	m.HTTPDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
		Help: "HTTP request latency.",
		// Buckets are upper bounds ("le" = less-or-equal). Pick them around your SLO:
		// if the target is "99% under 250ms", you need a bucket edge at 0.25.
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"method", "route"})

	m.HTTPInFlight = f.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "http", Name: "requests_in_flight",
		Help: "Requests being served right now.",
	})

	m.HTTPRespSize = f.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "http", Name: "response_size_bytes",
		Help:    "HTTP response body size.",
		Buckets: prometheus.ExponentialBuckets(64, 4, 7), // 64B .. 256KiB
	}, []string{"route"})

	m.OrdersCreated = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "orders", Name: "created_total",
		Help: "Orders accepted into the queue.",
	}, []string{"region", "payment_method"})

	m.OrdersFailed = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "orders", Name: "failed_total",
		Help: "Orders that failed, by reason.",
	}, []string{"reason"})

	m.OrderValue = f.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "orders", Name: "value_dollars",
		Help:    "Order value distribution.",
		Buckets: prometheus.ExponentialBuckets(5, 2, 10), // 5 .. 2560
	}, []string{"region"})

	m.CartItems = f.NewSummary(prometheus.SummaryOpts{
		Namespace: namespace, Subsystem: "orders", Name: "cart_items",
		Help: "Items per order (Summary, for comparison with Histogram).",
		// quantile -> allowed error. Computed in-process over a sliding window.
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	})

	// GaugeFunc: the value is computed AT SCRAPE TIME by calling the function.
	// Perfect for "how full is this queue right now".
	f.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "queue", Name: "depth",
		Help: "Orders waiting in the processing queue.",
	}, func() float64 { return float64(queueLen()) })

	f.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "queue", Name: "capacity",
		Help: "Maximum queue size.",
	}, func() float64 { return float64(queueCap()) })

	f.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "workers", Name: "total",
		Help: "Configured worker goroutines.",
	}).Set(float64(workers))

	m.WorkersBusy = f.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "workers", Name: "busy",
		Help: "Workers currently processing an order.",
	})

	m.StageDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "pipeline", Name: "stage_duration_seconds",
		Help:    "Time spent in each processing stage.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 10),
	}, []string{"stage"})

	m.OrdersProcessed = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "pipeline", Name: "orders_processed_total",
		Help: "Orders that left the pipeline, by outcome.",
	}, []string{"outcome"})

	m.PaymentRequests = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "payment", Name: "requests_total",
		Help: "Calls to payment providers.",
	}, []string{"provider", "outcome"})

	m.PaymentLatency = f.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "payment", Name: "latency_seconds",
		Help:    "Payment provider latency.",
		Buckets: []float64{.025, .05, .1, .2, .4, .8, 1.6, 3.2},
	}, []string{"provider"})

	m.CacheRequests = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "cache", Name: "requests_total",
		Help: "Product cache lookups.",
	}, []string{"result"}) // hit | miss

	m.DBQueryDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "db", Name: "query_duration_seconds",
		Help:    "Simulated database query latency.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
	}, []string{"query"})

	m.InventoryLevel = f.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "inventory", Name: "stock_level",
		Help: "Units in stock per SKU (bounded set of SKUs, so OK as a label).",
	}, []string{"sku"})

	m.ChaosLatency = f.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "chaos", Name: "injected_latency_seconds",
		Help: "Extra latency injected via /admin/chaos.",
	})
	m.ChaosErrorRate = f.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "chaos", Name: "injected_error_ratio",
		Help: "Fraction of requests forced to fail via /admin/chaos.",
	})

	// The "info" pattern: a gauge always set to 1 whose labels carry metadata.
	// Join it in PromQL to show which version each instance runs.
	f.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info",
		Help: "Build metadata. Value is always 1.",
	}, []string{"version", "goversion"}).WithLabelValues(version, runtime.Version()).Set(1)

	// NOTE: we never add an "instance" or "hostname" label ourselves.
	// Prometheus attaches job and instance to every series it scrapes, based on
	// where it found the target (service discovery). The app stays unaware of it.
	return m
}
