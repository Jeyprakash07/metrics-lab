package main

import (
	"log"
	"math/rand/v2"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
)

// runBatch simulates a nightly "reconcile payments" job.
//
// WHY PUSH HERE? A batch job may live for 3 seconds. Prometheus scrapes every
// 15s, so it would almost never catch the job alive. Instead the job pushes its
// final numbers to the Pushgateway, which holds them, and Prometheus scrapes the
// Pushgateway like any other target.
//
// The Pushgateway is NOT a general push pipeline: use it only for service-level
// batch jobs. Long-running services must expose /metrics and be pulled.
func runBatch(gateway string, every time.Duration) {
	for {
		runOnce(gateway)
		time.Sleep(every)
	}
}

func runOnce(gateway string) {
	reg := prometheus.NewRegistry()
	duration := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shop_batch_duration_seconds", Help: "How long the last run took.",
	})
	records := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shop_batch_records_processed", Help: "Records reconciled in the last run.",
	})
	lastSuccess := prometheus.NewGauge(prometheus.GaugeOpts{
		// The single most useful batch metric: alert on time() - this > threshold.
		Name: "shop_batch_last_success_timestamp_seconds", Help: "Unix time of the last successful run.",
	})
	reg.MustRegister(duration, records, lastSuccess)

	start := time.Now()
	n := 1000 + rand.IntN(5000)
	time.Sleep(time.Duration(n/10) * time.Millisecond) // "work"
	records.Set(float64(n))
	duration.Set(time.Since(start).Seconds())

	failed := rand.Float64() < 0.1
	if !failed {
		lastSuccess.SetToCurrentTime()
	}

	pusher := push.New(gateway, "reconcile_payments").Gatherer(reg)
	if failed {
		// On failure only push duration/records, keep the previous success timestamp.
		pusher = push.New(gateway, "reconcile_payments").Collector(duration).Collector(records)
		log.Printf("batch run failed (simulated), records=%d", n)
	}
	// Add = PUT semantics per metric name; grouping key here is job="reconcile_payments".
	if err := pusher.Add(); err != nil {
		log.Printf("push failed: %v", err)
		return
	}
	log.Printf("batch pushed: records=%d took=%s ok=%v", n, time.Since(start).Round(time.Millisecond), !failed)
}
