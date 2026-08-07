package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/XSAM/otelsql"
	_ "github.com/lib/pq"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/coroot/rca-lab/services/product-catalog/proto"
)

var (
	db                 *sql.DB
	recommendationStub pb.RecommendationServiceClient
)

type Product struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Price       float64         `json:"price"`
	SKU         string          `json:"sku"`
	Metadata    json.RawMessage `json:"metadata"`
	ImageURL    string          `json:"image_url"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func main() {
	ctx := context.Background()

	shutdownTelemetry, err := initTelemetry(ctx, "product-catalog")
	if err != nil {
		slog.Error("Failed to initialize telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdownTelemetry(context.Background())

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/products?sslmode=disable"
	}

	db, err = otelsql.Open("postgres", dbURL,
		otelsql.WithAttributes(semconv.DBSystemNamePostgreSQL),
	)
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if _, err := otelsql.RegisterDBStatsMetrics(db,
		otelsql.WithAttributes(semconv.DBSystemNamePostgreSQL),
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

	recAddr := os.Getenv("RECOMMENDATION_SERVICE_ADDR")
	if recAddr == "" {
		recAddr = "recommendation-service:9000"
	}
	// Use grpc.Dial with WithBlock so the underlying TCP connect happens here at startup;
	// grpc.NewClient is lazy and defers the connect until the first RPC.
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()
	recConn, err := grpc.DialContext(dialCtx, recAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithBlock(),
	)
	if err != nil {
		slog.Error("Failed to connect to recommendation service", "error", err)
		os.Exit(1)
	}
	defer recConn.Close()
	recommendationStub = pb.NewRecommendationServiceClient(recConn)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/products", productsHandler)
	mux.HandleFunc("/products/search", searchHandler)
	mux.HandleFunc("/products/category/", categoryHandler)
	mux.HandleFunc("/products/recommendations/", recommendationsHandler)
	mux.HandleFunc("/products/", productByIDHandler)

	slog.Info("Product catalog service listening on :8080")
	if err := http.ListenAndServe(":8080", otelhttp.NewHandler(mux, "product-catalog")); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

func recommendationsHandler(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimPrefix(r.URL.Path, "/products/recommendations/")
	if userID == "" || strings.Contains(userID, "/") {
		http.Error(w, "bad user id", http.StatusBadRequest)
		return
	}
	limit := int32(10)
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 50 {
		limit = int32(l)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := recommendationStub.GetRecommendations(ctx, &pb.GetRecommendationsRequest{UserId: userID, Limit: limit})
	if err != nil {
		slog.ErrorContext(r.Context(), "recommendations rpc failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"user_id": userID, "product_ids": resp.ProductIds})
}

func initDB(ctx context.Context) {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS products (
			id SERIAL PRIMARY KEY,
			name VARCHAR(500) NOT NULL,
			description TEXT,
			category VARCHAR(100),
			price DECIMAL(10,2),
			sku VARCHAR(100),
			metadata JSONB,
			image_url VARCHAR(500),
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)
	`)
	if err != nil {
		slog.Error("Failed to create table", "error", err)
		os.Exit(1)
	}

	// Ensure pg_trgm extension and trigram index for ILIKE search
	db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm`)
	db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_products_name_trgm ON products USING gin (name gin_trgm_ops)`)

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
	if err := db.PingContext(r.Context()); err != nil {
		writeError(w, 503, "database unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func productsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listProducts(w, r)
	case http.MethodPost:
		createProduct(w, r)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func listProducts(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	rows, err := db.QueryContext(r.Context(),
		"SELECT id, name, description, category, price, sku, metadata, image_url, created_at, updated_at FROM products ORDER BY id LIMIT $1 OFFSET $2",
		limit, offset,
	)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("query error: %v", err))
		return
	}
	defer rows.Close()

	products := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Category, &p.Price, &p.SKU, &p.Metadata, &p.ImageURL, &p.CreatedAt, &p.UpdatedAt); err != nil {
			writeError(w, 500, fmt.Sprintf("scan error: %v", err))
			return
		}
		products = append(products, p)
	}

	writeJSON(w, 200, map[string]interface{}{
		"products": products,
		"page":     page,
		"limit":    limit,
	})
}

func createProduct(w http.ResponseWriter, r *http.Request) {
	var p Product
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "invalid JSON")
		return
	}

	err := db.QueryRowContext(r.Context(),
		"INSERT INTO products (name, description, category, price, sku, metadata, image_url) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at, updated_at",
		p.Name, p.Description, p.Category, p.Price, p.SKU, p.Metadata, p.ImageURL,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("insert error: %v", err))
		return
	}

	writeJSON(w, 201, p)
}

func productByIDHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/products/"), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeError(w, 400, "invalid product ID")
		return
	}

	var p Product
	err = db.QueryRowContext(r.Context(),
		"SELECT id, name, description, category, price, sku, metadata, image_url, created_at, updated_at FROM products WHERE id = $1",
		id,
	).Scan(&p.ID, &p.Name, &p.Description, &p.Category, &p.Price, &p.SKU, &p.Metadata, &p.ImageURL, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		writeError(w, 404, "product not found")
		return
	}
	if err != nil {
		writeError(w, 500, fmt.Sprintf("query error: %v", err))
		return
	}

	writeJSON(w, 200, p)
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, 400, "query parameter 'q' is required")
		return
	}

	rows, err := db.QueryContext(r.Context(),
		"SELECT id, name, description, category, price, sku, metadata, image_url, created_at, updated_at FROM products WHERE name ILIKE '%' || $1 || '%' LIMIT 50",
		q,
	)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("search error: %v", err))
		return
	}
	defer rows.Close()

	products := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Category, &p.Price, &p.SKU, &p.Metadata, &p.ImageURL, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		products = append(products, p)
	}

	writeJSON(w, 200, map[string]interface{}{
		"products": products,
		"query":    q,
	})
}

func categoryHandler(w http.ResponseWriter, r *http.Request) {
	category := strings.TrimPrefix(r.URL.Path, "/products/category/")
	if category == "" {
		writeError(w, 400, "category is required")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	rows, err := db.QueryContext(r.Context(),
		"SELECT id, name, description, category, price, sku, metadata, image_url, created_at, updated_at FROM products WHERE category = $1 ORDER BY id LIMIT $2 OFFSET $3",
		category, limit, offset,
	)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("query error: %v", err))
		return
	}
	defer rows.Close()

	products := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Category, &p.Price, &p.SKU, &p.Metadata, &p.ImageURL, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		products = append(products, p)
	}

	writeJSON(w, 200, map[string]interface{}{
		"products": products,
		"category": category,
		"page":     page,
		"limit":    limit,
	})
}
