// order-service: a toy e-commerce backend built to teach Prometheus + Grafana.
//
// One binary, three modes:
//
//	-mode=server   the service itself. Exposes GET /metrics for Prometheus to PULL.
//	-mode=loadgen  sends realistic traffic at the servers so the graphs move.
//	-mode=batch    a short-lived job that PUSHES its metrics to a Pushgateway
//	               (the one case where push is the right answer).
package main

import (
	"flag"
	"log"
	"math"
	"time"
)

var version = "1.4.2" // usually injected with -ldflags "-X main.version=..."

func main() {
	mode := flag.String("mode", "server", "server | loadgen | batch")
	addr := flag.String("addr", ":8080", "server listen address")
	workers := flag.Int("workers", 8, "server: pipeline workers")
	queue := flag.Int("queue", 200, "server: queue capacity")
	target := flag.String("target", "http://order-service:8080", "loadgen: base URL")
	rps := flag.Float64("rps", 25, "loadgen: requests per second")
	pushURL := flag.String("pushgateway", "http://pushgateway:9091", "batch: pushgateway URL")
	interval := flag.Duration("interval", 60*time.Second, "batch: how often the job 'runs'")
	flag.Parse()

	switch *mode {
	case "server":
		runServer(*addr, version, *workers, *queue)
	case "loadgen":
		runLoadgen(*target, *rps)
	case "batch":
		runBatch(*pushURL, *interval)
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

func float64bits(f float64) uint64     { return math.Float64bits(f) }
func float64frombits(b uint64) float64 { return math.Float64frombits(b) }
