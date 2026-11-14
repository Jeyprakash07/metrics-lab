package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type OrderRequest struct {
	Region        string `json:"region"`
	PaymentMethod string `json:"payment_method"`
	Items         []Item `json:"items"`
}

type Order struct {
	ID     int64   `json:"id"`
	Region string  `json:"region"`
	Method string  `json:"payment_method"`
	Items  []Item  `json:"items"`
	Value  float64 `json:"value"`
	Status string  `json:"status"`
}

var (
	SKUs      = []string{"SKU-001", "SKU-002", "SKU-003", "SKU-004", "SKU-005", "SKU-006", "SKU-007", "SKU-008", "SKU-009", "SKU-010"}
	prices    = map[string]float64{}
	regions   = []string{"us-east", "us-west", "eu-central", "ap-south"}
	payMethod = []string{"card", "paypal", "wallet"}
	providers = map[string]string{"card": "stripe-sim", "paypal": "paypal-sim", "wallet": "stripe-sim"}
)

func init() {
	for i, s := range SKUs {
		prices[s] = float64(5 + i*12)
	}
}

type Server struct {
	m       *Metrics
	queue   chan *Order
	nextID  atomic.Int64
	mu      sync.RWMutex
	orders  map[int64]*Order
	stockMu sync.Mutex
	stock   map[string]int

	// chaos knobs
	extraLatency atomic.Int64  // nanoseconds
	errorRate    atomic.Uint64 // float64 bits
}

func runServer(addr, version string, workers, queueSize int) {
	s := &Server{
		queue:  make(chan *Order, queueSize),
		orders: map[int64]*Order{},
		stock:  map[string]int{},
	}
	s.m = NewMetrics(version,
		func() int { return len(s.queue) },
		func() int { return cap(s.queue) },
		workers)

	for _, sku := range SKUs {
		s.stock[sku] = 500
		s.m.InventoryLevel.WithLabelValues(sku).Set(500)
	}
	for i := 0; i < workers; i++ {
		go s.worker()
	}
	go s.restocker()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", s.createOrder)
	mux.HandleFunc("GET /orders/{id}", s.getOrder)
	mux.HandleFunc("GET /products/{sku}", s.getProduct)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /admin/chaos", s.chaos)

	// THE endpoint. Prometheus sends GET /metrics every scrape_interval.
	// promhttp.HandlerFor renders the registry in the text exposition format
	// (or protobuf/OpenMetrics if the scraper asks for it via the Accept header).
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.m.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true, // needed to expose exemplars and _created timestamps
	}))

	log.Printf("order-service %s listening on %s (%d workers, queue %d)", version, addr, workers, queueSize)
	log.Fatal(http.ListenAndServe(addr, s.instrument(mux)))
}

// ---------------- HTTP instrumentation middleware ----------------

type statusRecorder struct {
	http.ResponseWriter
	code  int
	bytes int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }
func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" { // don't measure the scraper itself
			next.ServeHTTP(w, r)
			return
		}
		s.m.HTTPInFlight.Inc()
		defer s.m.HTTPInFlight.Dec()

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)

		// r.Pattern is the ROUTE TEMPLATE ("GET /orders/{id}"), not the raw path
		// ("/orders/48213"). Using the raw path would create one time series per
		// order id: a cardinality explosion that can take Prometheus down.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.m.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(rec.code)).Inc()
		s.m.HTTPDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
		s.m.HTTPRespSize.WithLabelValues(route).Observe(float64(rec.bytes))
	})
}

// ---------------- handlers ----------------

func (s *Server) applyChaos(w http.ResponseWriter) bool {
	if d := s.extraLatency.Load(); d > 0 {
		time.Sleep(time.Duration(float64(d) * (0.5 + rand.Float64())))
	}
	if rate := float64frombits(s.errorRate.Load()); rate > 0 && rand.Float64() < rate {
		http.Error(w, "chaos: injected failure", http.StatusInternalServerError)
		return true
	}
	return false
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	if s.applyChaos(w) {
		s.m.OrdersFailed.WithLabelValues("chaos").Inc()
		return
	}
	var req OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Items) == 0 {
		s.m.OrdersFailed.WithLabelValues("bad_request").Inc()
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}
	value, items := 0.0, 0
	for _, it := range req.Items {
		p, ok := prices[it.SKU]
		if !ok {
			s.m.OrdersFailed.WithLabelValues("unknown_sku").Inc()
			http.Error(w, "unknown sku", http.StatusUnprocessableEntity)
			return
		}
		value += p * float64(it.Qty)
		items += it.Qty
	}
	o := &Order{ID: s.nextID.Add(1), Region: req.Region, Method: req.PaymentMethod, Items: req.Items, Value: value, Status: "queued"}

	select {
	case s.queue <- o:
	default:
		// Queue full: backpressure. This is "saturation" and it shows up as 503s.
		s.m.OrdersFailed.WithLabelValues("queue_full").Inc()
		http.Error(w, "busy, try later", http.StatusServiceUnavailable)
		return
	}

	s.mu.Lock()
	s.orders[o.ID] = o
	if len(s.orders) > 5000 { // keep memory bounded
		delete(s.orders, o.ID-5000)
	}
	s.mu.Unlock()

	s.m.OrdersCreated.WithLabelValues(req.Region, req.PaymentMethod).Inc()
	s.m.OrderValue.WithLabelValues(req.Region).Observe(value)
	s.m.CartItems.Observe(float64(items))

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(o)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	if s.applyChaos(w) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.dbQuery("select_order", 2*time.Millisecond)
	s.mu.RLock()
	o, ok := s.orders[id]
	s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(o)
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	if s.applyChaos(w) {
		return
	}
	sku := r.PathValue("sku")
	price, ok := prices[sku]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rand.Float64() < 0.85 {
		s.m.CacheRequests.WithLabelValues("hit").Inc()
	} else {
		s.m.CacheRequests.WithLabelValues("miss").Inc()
		s.dbQuery("select_product", 8*time.Millisecond)
	}
	s.stockMu.Lock()
	st := s.stock[sku]
	s.stockMu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"sku": sku, "price": price, "stock": st,
		"description": fmt.Sprintf("A lovely product %s with a long description to make the payload bigger.", sku)})
}

// POST /admin/chaos?latency_ms=300&error_rate=0.2   (both 0 to reset)
func (s *Server) chaos(w http.ResponseWriter, r *http.Request) {
	lat, _ := strconv.Atoi(r.URL.Query().Get("latency_ms"))
	er, _ := strconv.ParseFloat(r.URL.Query().Get("error_rate"), 64)
	s.extraLatency.Store(int64(time.Duration(lat) * time.Millisecond))
	s.errorRate.Store(float64bits(er))
	s.m.ChaosLatency.Set(float64(lat) / 1000)
	s.m.ChaosErrorRate.Set(er)
	fmt.Fprintf(w, "chaos set: latency=%dms error_rate=%.2f\n", lat, er)
}

// ---------------- background pipeline ----------------

func (s *Server) worker() {
	for o := range s.queue {
		s.m.WorkersBusy.Inc()
		outcome := s.process(o)
		s.m.OrdersProcessed.WithLabelValues(outcome).Inc()
		s.m.WorkersBusy.Dec()
	}
}

func (s *Server) process(o *Order) string {
	stage := func(name string, base time.Duration, fn func() error) error {
		t := time.Now()
		time.Sleep(jitter(base))
		err := fn()
		s.m.StageDuration.WithLabelValues(name).Observe(time.Since(t).Seconds())
		return err
	}
	setStatus := func(st string) { s.mu.Lock(); o.Status = st; s.mu.Unlock() }

	if err := stage("validate", 5*time.Millisecond, func() error { return nil }); err != nil {
		return "invalid"
	}
	if err := stage("reserve_stock", 10*time.Millisecond, func() error { return s.reserve(o) }); err != nil {
		setStatus("out_of_stock")
		s.m.OrdersFailed.WithLabelValues("out_of_stock").Inc()
		return "out_of_stock"
	}
	if err := stage("charge", 0, func() error { return s.charge(o) }); err != nil {
		setStatus("payment_failed")
		s.m.OrdersFailed.WithLabelValues("payment_declined").Inc()
		return "payment_failed"
	}
	_ = stage("ship", 20*time.Millisecond, func() error { return nil })
	setStatus("shipped")
	return "shipped"
}

func (s *Server) reserve(o *Order) error {
	s.stockMu.Lock()
	defer s.stockMu.Unlock()
	for _, it := range o.Items {
		if s.stock[it.SKU] < it.Qty {
			return errors.New("out of stock")
		}
	}
	for _, it := range o.Items {
		s.stock[it.SKU] -= it.Qty
		s.m.InventoryLevel.WithLabelValues(it.SKU).Set(float64(s.stock[it.SKU]))
	}
	return nil
}

func (s *Server) charge(o *Order) error {
	provider := providers[o.Method]
	if provider == "" {
		provider = "unknown"
	}
	t := time.Now()
	base := 80 * time.Millisecond
	if provider == "paypal-sim" {
		base = 180 * time.Millisecond // a slower dependency, visible in the latency panel
	}
	lat := jitter(base)
	if rand.Float64() < 0.01 {
		lat += 1500 * time.Millisecond // rare long tail: this is why you look at p99, not averages
	}
	time.Sleep(lat)
	s.m.PaymentLatency.WithLabelValues(provider).Observe(time.Since(t).Seconds())

	switch r := rand.Float64(); {
	case r < 0.03:
		s.m.PaymentRequests.WithLabelValues(provider, "declined").Inc()
		return errors.New("declined")
	case r < 0.04:
		s.m.PaymentRequests.WithLabelValues(provider, "error").Inc()
		return errors.New("provider error")
	default:
		s.m.PaymentRequests.WithLabelValues(provider, "approved").Inc()
		return nil
	}
}

func (s *Server) restocker() {
	for range time.Tick(20 * time.Second) {
		s.stockMu.Lock()
		for _, sku := range SKUs {
			if s.stock[sku] < 100 {
				s.stock[sku] += 400
				s.m.InventoryLevel.WithLabelValues(sku).Set(float64(s.stock[sku]))
			}
		}
		s.stockMu.Unlock()
	}
}

func (s *Server) dbQuery(name string, base time.Duration) {
	t := time.Now()
	time.Sleep(jitter(base))
	s.m.DBQueryDuration.WithLabelValues(name).Observe(time.Since(t).Seconds())
}

func jitter(d time.Duration) time.Duration {
	if d == 0 {
		return 0
	}
	return time.Duration(float64(d) * (0.5 + rand.ExpFloat64()*0.5))
}
