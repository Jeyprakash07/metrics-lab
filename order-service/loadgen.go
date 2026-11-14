package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"time"
)

// runLoadgen produces a traffic mix with a slow "daily" wave so rate graphs
// are not flat lines. It disables keep-alive so Docker DNS spreads requests
// across all replicas of order-service.
func runLoadgen(target string, rps float64) {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	log.Printf("loadgen -> %s at ~%.0f rps", target, rps)
	start := time.Now()
	for {
		// a 10-minute sine wave between 50% and 150% of the base rate
		wave := 1 + 0.5*math.Sin(2*math.Pi*time.Since(start).Minutes()/10)
		time.Sleep(time.Duration(float64(time.Second) / (rps * wave)))
		go hit(client, target)
	}
}

func hit(c *http.Client, base string) {
	var req *http.Request
	switch r := rand.Float64(); {
	case r < 0.50:
		sku := SKUs[rand.IntN(len(SKUs))]
		if rand.Float64() < 0.02 {
			sku = "SKU-404"
		}
		req, _ = http.NewRequest("GET", base+"/products/"+sku, nil)
	case r < 0.80:
		n := 1 + rand.IntN(4)
		items := make([]Item, n)
		for i := range items {
			items[i] = Item{SKU: SKUs[rand.IntN(len(SKUs))], Qty: 1 + rand.IntN(3)}
		}
		body, _ := json.Marshal(OrderRequest{
			Region:        regions[weighted([]float64{.45, .2, .25, .1})],
			PaymentMethod: payMethod[weighted([]float64{.7, .2, .1})],
			Items:         items,
		})
		if rand.Float64() < 0.02 {
			body = []byte("{not json")
		}
		req, _ = http.NewRequest("POST", base+"/orders", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	case r < 0.97:
		req, _ = http.NewRequest("GET", fmt.Sprintf("%s/orders/%d", base, 1+rand.IntN(2000)), nil)
	default:
		req, _ = http.NewRequest("GET", base+"/healthz", nil)
	}
	resp, err := c.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func weighted(w []float64) int {
	r := rand.Float64()
	for i, p := range w {
		if r < p {
			return i
		}
		r -= p
	}
	return len(w) - 1
}
