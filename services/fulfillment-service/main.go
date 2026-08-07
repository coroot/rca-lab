package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/XSAM/otelsql"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const (
	consumeTopic  = "order-events"
	produceTopic  = "shipment-events"
	consumerGroup = "fulfillment"
	maxAttempts   = 3
	retryBackoff  = 500 * time.Millisecond
)

var (
	db           *sql.DB
	kafka        *kgo.Client
	kotelTracer  *kotel.Tracer
	inventoryURL string
	httpClient   = &http.Client{
		Timeout:   10 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
)

type OrderItem struct {
	ProductID int64   `json:"productId"`
	Quantity  int     `json:"quantity"`
	Price     float64 `json:"price"`
}

type OrderEvent struct {
	EventType   string      `json:"eventType"`
	OrderID     int64       `json:"orderId"`
	UserID      string      `json:"userId"`
	Items       []OrderItem `json:"items"`
	TotalAmount float64     `json:"totalAmount"`
	CreatedAt   string      `json:"createdAt"`
}

type ShipmentEvent struct {
	EventType  string `json:"eventType"`
	OrderID    int64  `json:"orderId"`
	ShipmentID int64  `json:"shipmentId"`
	Status     string `json:"status"`
	CreatedAt  string `json:"createdAt"`
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()

	shutdownTelemetry, err := initTelemetry(ctx, "fulfillment-service")
	if err != nil {
		slog.Error("Failed to initialize telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdownTelemetry(context.Background())

	// Build the DSN from discrete parts via mysql.Config so the
	// operator-generated password (which may contain characters that break a
	// raw user:pass@tcp(...) DSN) is treated as opaque.
	dbCfg := mysqldriver.NewConfig()
	dbCfg.Net = "tcp"
	dbCfg.Addr = getenv("MYSQL_HOST", "mysql-haproxy") + ":" + getenv("MYSQL_PORT", "3306")
	dbCfg.User = getenv("MYSQL_USER", "orders")
	dbCfg.Passwd = os.Getenv("MYSQL_PASSWORD")
	dbCfg.DBName = getenv("MYSQL_DATABASE", "orders")
	dbCfg.ParseTime = true

	db, err = otelsql.Open("mysql", dbCfg.FormatDSN(),
		otelsql.WithAttributes(semconv.DBSystemNameMySQL),
	)
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if _, err := otelsql.RegisterDBStatsMetrics(db,
		otelsql.WithAttributes(semconv.DBSystemNameMySQL),
	); err != nil {
		slog.Error("Failed to register db stats metrics", "error", err)
		os.Exit(1)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	for i := 0; i < 30; i++ {
		if err = db.PingContext(ctx); err == nil {
			break
		}
		slog.Info("Waiting for database...", "attempt", i+1, "max_attempts", 30)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		slog.Error("Database not available", "error", err)
		os.Exit(1)
	}

	initDB(ctx)

	inventoryURL = os.Getenv("INVENTORY_SERVICE_URL")
	if inventoryURL == "" {
		inventoryURL = "http://inventory-service:8080"
	}
	inventoryURL = strings.TrimSuffix(inventoryURL, "/")

	bootstrap := os.Getenv("KAFKA_BOOTSTRAP_SERVERS")
	if bootstrap == "" {
		bootstrap = "kafka-kafka-bootstrap:9092"
	}

	kotelTracer = kotel.NewTracer()
	kotelService := kotel.NewKotel(
		kotel.WithTracer(kotelTracer),
		kotel.WithMeter(kotel.NewMeter()),
	)

	kafka, err = kgo.NewClient(
		kgo.SeedBrokers(strings.Split(bootstrap, ",")...),
		kgo.ConsumerGroup(consumerGroup),
		kgo.ConsumeTopics(consumeTopic),
		kgo.AutoCommitMarks(),
		kgo.WithHooks(kotelService.Hooks()...),
	)
	if err != nil {
		slog.Error("Failed to create kafka client", "error", err)
		os.Exit(1)
	}
	defer kafka.Close()

	for i := 0; i < 30; i++ {
		if err = kafka.Ping(ctx); err == nil {
			break
		}
		slog.Info("Waiting for kafka...", "attempt", i+1, "max_attempts", 30)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		slog.Error("Kafka not available", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	server := &http.Server{
		Addr:    ":8080",
		Handler: otelhttp.NewHandler(mux, "fulfillment-service"),
	}
	go func() {
		slog.Info("Fulfillment service health endpoint listening on :8080")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Server failed", "error", err)
			os.Exit(1)
		}
	}()

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	slog.Info("Consuming order events", "topic", consumeTopic, "group", consumerGroup, "brokers", bootstrap)
	consumeLoop(runCtx)

	slog.Info("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown failed", "error", err)
	}
	if err := kafka.CommitMarkedOffsets(shutdownCtx); err != nil {
		slog.Error("Failed to commit marked offsets", "error", err)
	}
	if err := kafka.Flush(shutdownCtx); err != nil {
		slog.Error("Failed to flush producer", "error", err)
	}
}

// consumeLoop polls order-events until ctx is cancelled (SIGTERM), processing
// each record and marking it for commit so a poison message never wedges the
// partition.
func consumeLoop(ctx context.Context) {
	for {
		fetches := kafka.PollFetches(ctx)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			if !errors.Is(err, context.Canceled) {
				slog.Error("Fetch error", "topic", topic, "partition", partition, "error", err)
			}
		})
		fetches.EachRecord(func(rec *kgo.Record) {
			processRecord(rec)
			kafka.MarkCommitRecords(rec)
		})
	}
}

func processRecord(rec *kgo.Record) {
	ctx, span := kotelTracer.WithProcessSpan(rec)
	defer span.End()

	var event OrderEvent
	if err := json.Unmarshal(rec.Value, &event); err != nil {
		slog.ErrorContext(ctx, "Failed to decode order event, skipping", "error", err)
		return
	}
	if event.EventType != "OrderCreated" {
		return
	}

	for _, item := range event.Items {
		if err := reserveInventory(ctx, item.ProductID, item.Quantity); err != nil {
			slog.ErrorContext(ctx, "Inventory reservation failed, skipping order",
				"order_id", event.OrderID, "product_id", item.ProductID, "error", err)
			return
		}
	}

	shipmentID, err := createShipment(ctx, event.OrderID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create shipment, skipping order",
			"order_id", event.OrderID, "error", err)
		return
	}

	if err := publishShipmentCreated(ctx, event.OrderID, shipmentID); err != nil {
		slog.ErrorContext(ctx, "Failed to publish shipment event",
			"order_id", event.OrderID, "shipment_id", shipmentID, "error", err)
		return
	}

	slog.InfoContext(ctx, "Shipment created", "order_id", event.OrderID, "shipment_id", shipmentID)
}

// reserveInventory PUTs a reservation to the inventory service. A 409
// (insufficient stock) is a business outcome: it is logged as a warning and
// treated as success so fulfillment continues. Transport failures and other
// HTTP errors are retried a few times with backoff before giving up.
func reserveInventory(ctx context.Context, productID int64, quantity int) error {
	url := fmt.Sprintf("%s/inventory/%d/reserve", inventoryURL, productID)
	payload, err := json.Marshal(map[string]int{"quantity": quantity})
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt-1) * retryBackoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusConflict:
			slog.WarnContext(ctx, "Insufficient stock, continuing fulfillment",
				"product_id", productID, "quantity", quantity)
			return nil
		default:
			lastErr = fmt.Errorf("inventory service returned status %d: %s",
				resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	return lastErr
}

func createShipment(ctx context.Context, orderID int64) (int64, error) {
	res, err := db.ExecContext(ctx,
		"INSERT INTO shipments (order_id, status) VALUES (?, 'CREATED')",
		orderID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func publishShipmentCreated(ctx context.Context, orderID, shipmentID int64) error {
	event := ShipmentEvent{
		EventType:  "ShipmentCreated",
		OrderID:    orderID,
		ShipmentID: shipmentID,
		Status:     "CREATED",
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	value, err := json.Marshal(event)
	if err != nil {
		return err
	}
	rec := &kgo.Record{
		Topic: produceTopic,
		Key:   []byte(strconv.FormatInt(orderID, 10)),
		Value: value,
	}
	return kafka.ProduceSync(ctx, rec).FirstErr()
}

func initDB(ctx context.Context) {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS shipments (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			order_id BIGINT,
			status VARCHAR(32) DEFAULT 'PENDING',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		slog.Error("Failed to create table", "error", err)
		os.Exit(1)
	}
	slog.Info("Database initialized")
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		writeError(w, 503, "database unavailable")
		return
	}
	if err := kafka.Ping(ctx); err != nil {
		writeError(w, 503, "kafka unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
