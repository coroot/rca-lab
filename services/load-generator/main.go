package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var (
	gatewayURL = getEnv("GATEWAY_URL", "http://api-gateway:8080")
	rpsPerSvc  = getEnvInt("RPS_PER_SERVICE", 20)
	client     = &http.Client{
		Timeout: 10 * time.Second,
		Transport: otelhttp.NewTransport(&http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 200,
			MaxConnsPerHost:     200,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}),
	}
)

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type scenario struct {
	name   string
	weight int
	fn     func() error
}

var (
	totalReqs   int64
	totalErrs   int64
	categories  = []string{"electronics", "clothing", "books", "home", "sports", "toys", "food", "beauty", "automotive", "garden"}
	searchTerms = []string{"wireless", "organic", "premium", "vintage", "smart", "portable", "eco", "professional", "mini", "ultra"}
	userIDs     = make([]string, 100)
)

func init() {
	for i := range userIDs {
		userIDs[i] = fmt.Sprintf("user-%d", rand.Intn(10000))
	}
}

func randomUserID() string {
	return userIDs[rand.Intn(len(userIDs))]
}

func doGet(url string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

var bufPool = sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

func doPost(url string, body interface{}) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return nil, err
	}
	resp, err := client.Post(url, "application/json", buf)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return respBody, fmt.Errorf("status %d", resp.StatusCode)
	}
	return respBody, nil
}

func doPut(url string, body interface{}) error {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return err
	}
	req, _ := http.NewRequest("PUT", url, buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func doDelete(url string) error {
	req, _ := http.NewRequest("DELETE", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// Product scenarios
func listProducts() error {
	page := rand.Intn(100) + 1
	return doGet(fmt.Sprintf("%s/api/products?page=%d&limit=20", gatewayURL, page))
}

func getProduct() error {
	id := rand.Intn(500000) + 1
	return doGet(fmt.Sprintf("%s/api/products/%d", gatewayURL, id))
}

func searchProducts() error {
	term := searchTerms[rand.Intn(len(searchTerms))]
	return doGet(fmt.Sprintf("%s/api/products/search?q=%s", gatewayURL, term))
}

func getProductsByCategory() error {
	cat := categories[rand.Intn(len(categories))]
	return doGet(fmt.Sprintf("%s/api/products/category/%s", gatewayURL, cat))
}

// Cart scenarios
func getCart() error {
	return doGet(fmt.Sprintf("%s/api/cart/%s", gatewayURL, randomUserID()))
}

func addToCart() error {
	uid := randomUserID()
	item := map[string]interface{}{
		"product_id": fmt.Sprintf("%d", rand.Intn(500000)+1),
		"quantity":   rand.Intn(5) + 1,
		"price":      float64(rand.Intn(10000)) / 100.0,
		"name":       fmt.Sprintf("Product %d", rand.Intn(500000)),
	}
	_, err := doPost(fmt.Sprintf("%s/api/cart/%s/items", gatewayURL, uid), item)
	return err
}

func removeFromCart() error {
	uid := randomUserID()
	pid := rand.Intn(500000) + 1
	return doDelete(fmt.Sprintf("%s/api/cart/%s/items/%d", gatewayURL, uid, pid))
}

// Order scenarios
func listOrders() error {
	page := rand.Intn(50) + 1
	return doGet(fmt.Sprintf("%s/api/orders?page=%d&size=20", gatewayURL, page))
}

func getOrder() error {
	id := rand.Intn(100000) + 1
	return doGet(fmt.Sprintf("%s/api/orders/%d", gatewayURL, id))
}

func createOrder() error {
	order := map[string]interface{}{
		"user_id": randomUserID(),
		"items": []map[string]interface{}{
			{"product_id": fmt.Sprintf("%d", rand.Intn(500000)+1), "quantity": rand.Intn(3) + 1, "price": float64(rand.Intn(5000)) / 100.0, "name": "Product A"},
			{"product_id": fmt.Sprintf("%d", rand.Intn(500000)+1), "quantity": rand.Intn(2) + 1, "price": float64(rand.Intn(3000)) / 100.0, "name": "Product B"},
		},
		"total":            float64(rand.Intn(10000)) / 100.0,
		"shipping_address": fmt.Sprintf("%d Main St, City %d, ST %05d", rand.Intn(9999)+1, rand.Intn(100), rand.Intn(99999)),
	}
	_, err := doPost(fmt.Sprintf("%s/api/orders", gatewayURL), order)
	return err
}

// createOrderAndAwaitFulfillment creates an order like createOrder, then polls
// it via the gateway until the async fulfillment pipeline (order-events ->
// fulfillment-service -> shipment-events) marks it FULFILLED.
func createOrderAndAwaitFulfillment() error {
	order := map[string]interface{}{
		"user_id": randomUserID(),
		"items": []map[string]interface{}{
			{"product_id": fmt.Sprintf("%d", rand.Intn(500000)+1), "quantity": rand.Intn(3) + 1, "price": float64(rand.Intn(5000)) / 100.0, "name": "Product A"},
			{"product_id": fmt.Sprintf("%d", rand.Intn(500000)+1), "quantity": rand.Intn(2) + 1, "price": float64(rand.Intn(3000)) / 100.0, "name": "Product B"},
		},
		"total":            float64(rand.Intn(10000)) / 100.0,
		"shipping_address": fmt.Sprintf("%d Main St, City %d, ST %05d", rand.Intn(9999)+1, rand.Intn(100), rand.Intn(99999)),
	}
	respBody, err := doPost(fmt.Sprintf("%s/api/orders", gatewayURL), order)
	if err != nil {
		return err
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil || created.ID == 0 {
		return fmt.Errorf("unexpected create order response: %s", respBody)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		resp, err := client.Get(fmt.Sprintf("%s/api/orders/%d", gatewayURL, created.ID))
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			continue
		}
		var current struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &current) == nil && current.Status == "FULFILLED" {
			return nil
		}
	}
	return fmt.Errorf("order %d not FULFILLED within 30s", created.ID)
}

func getUserOrders() error {
	return doGet(fmt.Sprintf("%s/api/orders/user/%s", gatewayURL, randomUserID()))
}

func updateOrderStatus() error {
	id := rand.Intn(100000) + 1
	statuses := []string{"CONFIRMED", "SHIPPED", "DELIVERED"}
	status := map[string]string{"status": statuses[rand.Intn(len(statuses))]}
	return doPut(fmt.Sprintf("%s/api/orders/%d/status", gatewayURL, id), status)
}

// Review scenarios
func listReviews() error {
	page := rand.Intn(100) + 1
	return doGet(fmt.Sprintf("%s/api/reviews?page=%d&limit=20", gatewayURL, page))
}

func getProductReviews() error {
	pid := rand.Intn(500000) + 1
	return doGet(fmt.Sprintf("%s/api/reviews/product/%d", gatewayURL, pid))
}

func getProductReviewStats() error {
	pid := rand.Intn(500000) + 1
	return doGet(fmt.Sprintf("%s/api/reviews/product/%d/stats", gatewayURL, pid))
}

func createReview() error {
	review := map[string]interface{}{
		"product_id":        fmt.Sprintf("%d", rand.Intn(500000)+1),
		"user_id":           randomUserID(),
		"rating":            rand.Intn(5) + 1,
		"title":             fmt.Sprintf("Review title %d", rand.Intn(10000)),
		"body":              fmt.Sprintf("This is a detailed review body text number %d with lots of words to simulate real review content.", rand.Intn(100000)),
		"pros":              []string{"good quality", "fast shipping"},
		"cons":              []string{"expensive"},
		"verified_purchase": rand.Intn(2) == 1,
	}
	_, err := doPost(fmt.Sprintf("%s/api/reviews", gatewayURL), review)
	return err
}

func searchReviews() error {
	term := searchTerms[rand.Intn(len(searchTerms))]
	return doGet(fmt.Sprintf("%s/api/reviews/search?q=%s", gatewayURL, term))
}

// Inventory scenarios
func listStock() error {
	page := rand.Intn(50) + 1
	return doGet(fmt.Sprintf("%s/api/inventory?page=%d&limit=20", gatewayURL, page))
}

func getStock() error {
	id := rand.Intn(500000) + 1
	return doGet(fmt.Sprintf("%s/api/inventory/%d", gatewayURL, id))
}

func reserveStock() error {
	payload := map[string]interface{}{
		"product_id": rand.Intn(500000) + 1,
		"user_id":    randomUserID(),
		"quantity":   rand.Intn(5) + 1,
	}
	_, err := doPost(fmt.Sprintf("%s/api/inventory/reserve", gatewayURL), payload)
	return err
}

func updateStock() error {
	id := rand.Intn(500000) + 1
	warehouses := []string{"main", "east", "west", "central"}
	payload := map[string]interface{}{
		"quantity":  rand.Intn(1000),
		"warehouse": warehouses[rand.Intn(len(warehouses))],
	}
	return doPut(fmt.Sprintf("%s/api/inventory/%d", gatewayURL, id), payload)
}

func getLowStock() error {
	return doGet(fmt.Sprintf("%s/api/inventory/low-stock?threshold=%d", gatewayURL, rand.Intn(20)+5))
}

// Payment scenarios
func listPayments() error {
	page := rand.Intn(50) + 1
	return doGet(fmt.Sprintf("%s/api/payments?page=%d&limit=20", gatewayURL, page))
}

func getUserPayments() error {
	return doGet(fmt.Sprintf("%s/api/payments/user/%s", gatewayURL, randomUserID()))
}

func processPayment() error {
	methods := []string{"card", "paypal", "crypto"}
	currencies := []string{"USD", "EUR", "GBP"}
	payment := map[string]interface{}{
		"order_id": fmt.Sprintf("order-%d", rand.Intn(100000)+1),
		"user_id":  randomUserID(),
		"amount":   float64(rand.Intn(50000)) / 100.0,
		"currency": currencies[rand.Intn(len(currencies))],
		"method":   methods[rand.Intn(len(methods))],
	}
	_, err := doPost(fmt.Sprintf("%s/api/payments", gatewayURL), payment)
	return err
}

type serviceScenarios struct {
	name      string
	scenarios []scenario
}

func allServices() []serviceScenarios {
	return []serviceScenarios{
		{
			name: "product-catalog",
			scenarios: []scenario{
				{"list-products", 30, listProducts},
				{"get-product", 30, getProduct},
				{"search-products", 20, searchProducts},
				{"get-by-category", 20, getProductsByCategory},
			},
		},
		{
			name: "cart-service",
			scenarios: []scenario{
				{"get-cart", 30, getCart},
				{"add-to-cart", 40, addToCart},
				{"remove-from-cart", 30, removeFromCart},
			},
		},
		{
			name: "order-service",
			scenarios: []scenario{
				{"list-orders", 20, listOrders},
				{"get-order", 20, getOrder},
				{"create-order", 25, createOrder},
				{"create-order-await-fulfillment", 12, createOrderAndAwaitFulfillment},
				{"get-user-orders", 20, getUserOrders},
				{"update-status", 15, updateOrderStatus},
			},
		},
		{
			name: "review-service",
			scenarios: []scenario{
				{"list-reviews", 20, listReviews},
				{"get-product-reviews", 20, getProductReviews},
				{"get-review-stats", 15, getProductReviewStats},
				{"create-review", 25, createReview},
				{"search-reviews", 20, searchReviews},
			},
		},
		{
			name: "inventory-service",
			scenarios: []scenario{
				{"list-stock", 25, listStock},
				{"get-stock", 25, getStock},
				{"reserve-stock", 20, reserveStock},
				{"update-stock", 15, updateStock},
				{"get-low-stock", 15, getLowStock},
			},
		},
		{
			name: "payment-service",
			scenarios: []scenario{
				{"list-payments", 30, listPayments},
				{"get-user-payments", 30, getUserPayments},
				{"process-payment", 40, processPayment},
			},
		},
		{
			name: "recommendation-service",
			scenarios: []scenario{
				{"via-product-catalog-go-grpc", 50, getRecommendationsViaProductCatalog},
				{"via-api-gateway-py-grpc", 50, getRecommendationsViaGateway},
			},
		},
	}
}

// /api/products/recommendations/<user> -> api-gateway -> product-catalog -> recommendation (Go gRPC)
func getRecommendationsViaProductCatalog() error {
	return doGet(fmt.Sprintf("%s/api/products/recommendations/%s?limit=%d", gatewayURL, randomUserID(), rand.Intn(20)+5))
}

// /api/recommendations/<user> -> api-gateway directly via grpc.aio (Python gRPC)
func getRecommendationsViaGateway() error {
	return doGet(fmt.Sprintf("%s/api/recommendations/%s?limit=%d", gatewayURL, randomUserID(), rand.Intn(20)+5))
}

func pickScenario(scenarios []scenario) scenario {
	total := 0
	for _, s := range scenarios {
		total += s.weight
	}
	r := rand.Intn(total)
	for _, s := range scenarios {
		r -= s.weight
		if r < 0 {
			return s
		}
	}
	return scenarios[0]
}

func runServiceLoad(svc serviceScenarios, rps int, wg *sync.WaitGroup) {
	defer wg.Done()
	interval := time.Second / time.Duration(rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Worker pool: use 2x RPS workers to handle concurrent requests without goroutine explosion
	workers := rps * 2
	if workers < 10 {
		workers = 10
	}
	work := make(chan scenario, workers)

	for i := 0; i < workers; i++ {
		go func() {
			for s := range work {
				atomic.AddInt64(&totalReqs, 1)
				if err := s.fn(); err != nil {
					atomic.AddInt64(&totalErrs, 1)
				}
			}
		}()
	}

	slog.Info("Starting load", "service", svc.name, "rps", rps, "workers", workers)

	for range ticker.C {
		s := pickScenario(svc.scenarios)
		select {
		case work <- s:
		default:
			// Workers busy, skip this tick to apply backpressure
		}
	}
}

func main() {
	ctx := context.Background()

	shutdownTelemetry, err := initTelemetry(ctx, "load-generator")
	if err != nil {
		slog.Error("failed to initialize telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdownTelemetry(context.Background())

	slog.Info("Load generator starting", "target", gatewayURL, "rps_per_service", rpsPerSvc)

	// Wait for gateway to be ready
	for i := 0; i < 60; i++ {
		resp, err := client.Get(gatewayURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				slog.Info("Gateway is ready")
				break
			}
		}
		slog.Info("Waiting for gateway...", "attempt", i+1, "max_attempts", 60)
		time.Sleep(5 * time.Second)
	}

	services := allServices()
	var wg sync.WaitGroup

	for _, svc := range services {
		wg.Add(1)
		go runServiceLoad(svc, rpsPerSvc, &wg)
	}

	// Stats reporter
	go func() {
		for {
			time.Sleep(10 * time.Second)
			reqs := atomic.LoadInt64(&totalReqs)
			errs := atomic.LoadInt64(&totalErrs)
			slog.Info("Stats", "total_requests", reqs, "total_errors", errs, "error_rate_pct", float64(errs)/float64(reqs+1)*100)
		}
	}()

	wg.Wait()
}
