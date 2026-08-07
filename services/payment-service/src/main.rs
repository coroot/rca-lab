use actix_web::{web, App, HttpServer, HttpResponse};
use chrono::{NaiveDateTime, Utc};
use opentelemetry::global;
use opentelemetry::trace::TracerProvider as _;
use opentelemetry::KeyValue;
use opentelemetry_appender_tracing::layer::OpenTelemetryTracingBridge;
use opentelemetry_instrumentation_actix_web::RequestMetrics;
use opentelemetry_sdk::logs::SdkLoggerProvider;
use opentelemetry_sdk::metrics::SdkMeterProvider;
use opentelemetry_sdk::propagation::TraceContextPropagator;
use opentelemetry_sdk::trace::SdkTracerProvider;
use opentelemetry_sdk::Resource;
use rand::Rng;
use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use sqlx::mysql::MySqlPoolOptions;
use sqlx::types::Json;
use sqlx::MySqlPool;
use std::env;
use std::time::Duration;
use tracing_actix_web::TracingLogger;
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::EnvFilter;
use uuid::Uuid;

#[derive(Debug, Serialize, Deserialize, sqlx::FromRow)]
struct Payment {
    id: String,
    order_id: String,
    user_id: String,
    amount: Decimal,
    currency: String,
    method: String,
    status: String,
    transaction_ref: String,
    #[sqlx(json)]
    metadata: serde_json::Value,
    created_at: NaiveDateTime,
    updated_at: NaiveDateTime,
}

#[derive(Debug, Deserialize)]
struct CreatePaymentRequest {
    order_id: String,
    user_id: String,
    amount: Decimal,
    currency: String,
    method: String,
}

#[derive(Debug, Deserialize)]
struct PaginationParams {
    page: Option<i64>,
    limit: Option<i64>,
}

struct AppState {
    pool: MySqlPool,
}

// --- Telemetry ---

struct Telemetry {
    tracer_provider: SdkTracerProvider,
    logger_provider: SdkLoggerProvider,
    meter_provider: SdkMeterProvider,
}

impl Telemetry {
    fn shutdown(&self) {
        if let Err(e) = self.tracer_provider.shutdown() {
            eprintln!("tracer provider shutdown error: {e}");
        }
        if let Err(e) = self.logger_provider.shutdown() {
            eprintln!("logger provider shutdown error: {e}");
        }
        if let Err(e) = self.meter_provider.shutdown() {
            eprintln!("meter provider shutdown error: {e}");
        }
    }
}

// OTLP exporters are configured entirely from the standard OTEL_* environment
// variables (OTEL_EXPORTER_OTLP_ENDPOINT and friends) by opentelemetry-otlp.
fn init_telemetry() -> Telemetry {
    global::set_text_map_propagator(TraceContextPropagator::new());

    let resource = Resource::builder()
        .with_service_name(
            env::var("OTEL_SERVICE_NAME").unwrap_or_else(|_| "payment-service".to_string()),
        )
        .with_attribute(KeyValue::new(
            opentelemetry_semantic_conventions::attribute::SERVICE_VERSION,
            env!("CARGO_PKG_VERSION"),
        ))
        .build();

    let span_exporter = opentelemetry_otlp::SpanExporter::builder()
        .with_tonic()
        .build()
        .expect("Failed to build OTLP span exporter");
    let tracer_provider = SdkTracerProvider::builder()
        .with_batch_exporter(span_exporter)
        .with_resource(resource.clone())
        .build();

    let log_exporter = opentelemetry_otlp::LogExporter::builder()
        .with_tonic()
        .build()
        .expect("Failed to build OTLP log exporter");
    let logger_provider = SdkLoggerProvider::builder()
        .with_batch_exporter(log_exporter)
        .with_resource(resource.clone())
        .build();

    let metric_exporter = opentelemetry_otlp::MetricExporter::builder()
        .with_tonic()
        .build()
        .expect("Failed to build OTLP metric exporter");
    let meter_provider = SdkMeterProvider::builder()
        .with_periodic_exporter(metric_exporter)
        .with_resource(resource)
        .build();

    global::set_tracer_provider(tracer_provider.clone());
    global::set_meter_provider(meter_provider.clone());

    let tracer = tracer_provider.tracer("payment-service");

    // Silence the HTTP/gRPC stacks used by the OTLP exporters themselves to
    // avoid telemetry-induced telemetry feedback.
    let env_filter = EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| EnvFilter::new("info,h2=warn,hyper=warn,tonic=warn,tower=warn"));

    tracing_subscriber::registry()
        .with(env_filter)
        .with(tracing_subscriber::fmt::layer().json())
        .with(tracing_opentelemetry::layer().with_tracer(tracer))
        .with(OpenTelemetryTracingBridge::new(&logger_provider))
        .init();

    Telemetry {
        tracer_provider,
        logger_provider,
        meter_provider,
    }
}

// --- Database queries ---

#[tracing::instrument(skip(pool))]
async fn db_ping(pool: &MySqlPool) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT 1").execute(pool).await?;
    Ok(())
}

#[tracing::instrument(skip(pool))]
async fn db_list_payments(
    pool: &MySqlPool,
    limit: i64,
    offset: i64,
) -> Result<Vec<Payment>, sqlx::Error> {
    sqlx::query_as::<_, Payment>(
        "SELECT id, order_id, user_id, amount, currency, method, status, transaction_ref, metadata, created_at, updated_at FROM payments ORDER BY created_at DESC LIMIT ? OFFSET ?"
    )
    .bind(limit)
    .bind(offset)
    .fetch_all(pool)
    .await
}

#[tracing::instrument(skip(pool))]
async fn db_get_payment(pool: &MySqlPool, id: &str) -> Result<Option<Payment>, sqlx::Error> {
    sqlx::query_as::<_, Payment>(
        "SELECT id, order_id, user_id, amount, currency, method, status, transaction_ref, metadata, created_at, updated_at FROM payments WHERE id = ?"
    )
    .bind(id)
    .fetch_optional(pool)
    .await
}

#[tracing::instrument(skip(pool, req, metadata))]
async fn db_insert_payment(
    pool: &MySqlPool,
    id: &str,
    req: &CreatePaymentRequest,
    status: &str,
    transaction_ref: &str,
    metadata: &serde_json::Value,
    now: NaiveDateTime,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO payments (id, order_id, user_id, amount, currency, method, status, transaction_ref, metadata, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)"
    )
    .bind(id)
    .bind(&req.order_id)
    .bind(&req.user_id)
    .bind(req.amount)
    .bind(&req.currency)
    .bind(&req.method)
    .bind(status)
    .bind(transaction_ref)
    .bind(Json(metadata))
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(())
}

#[tracing::instrument(skip(pool))]
async fn db_get_user_payments(
    pool: &MySqlPool,
    user_id: &str,
) -> Result<Vec<Payment>, sqlx::Error> {
    sqlx::query_as::<_, Payment>(
        "SELECT id, order_id, user_id, amount, currency, method, status, transaction_ref, metadata, created_at, updated_at FROM payments WHERE user_id = ? ORDER BY created_at DESC LIMIT 50"
    )
    .bind(user_id)
    .fetch_all(pool)
    .await
}

#[tracing::instrument(skip(pool))]
async fn db_refund_payment(pool: &MySqlPool, id: &str) -> Result<u64, sqlx::Error> {
    let result = sqlx::query("UPDATE payments SET status = 'refunded', updated_at = NOW() WHERE id = ? AND status = 'completed'")
        .bind(id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected())
}

// --- HTTP handlers ---

async fn health(data: web::Data<AppState>) -> HttpResponse {
    match db_ping(&data.pool).await {
        Ok(_) => HttpResponse::Ok().json(serde_json::json!({"status": "ok"})),
        Err(e) => HttpResponse::ServiceUnavailable()
            .json(serde_json::json!({"status": "error", "message": e.to_string()})),
    }
}

async fn list_payments(
    data: web::Data<AppState>,
    params: web::Query<PaginationParams>,
) -> HttpResponse {
    let page = params.page.unwrap_or(1).max(1);
    let limit = params.limit.unwrap_or(20).min(100);
    let offset = (page - 1) * limit;

    match db_list_payments(&data.pool, limit, offset).await {
        Ok(payments) => HttpResponse::Ok().json(serde_json::json!({
            "payments": payments,
            "page": page,
            "limit": limit,
        })),
        Err(e) => {
            tracing::error!("list_payments error: {}", e);
            HttpResponse::InternalServerError()
                .json(serde_json::json!({"error": e.to_string()}))
        }
    }
}

async fn get_payment(data: web::Data<AppState>, path: web::Path<String>) -> HttpResponse {
    let id = path.into_inner();
    match db_get_payment(&data.pool, &id).await {
        Ok(Some(payment)) => HttpResponse::Ok().json(payment),
        Ok(None) => HttpResponse::NotFound().json(serde_json::json!({"error": "payment not found"})),
        Err(e) => {
            tracing::error!("get_payment error: {}", e);
            HttpResponse::InternalServerError()
                .json(serde_json::json!({"error": e.to_string()}))
        }
    }
}

async fn process_payment(
    data: web::Data<AppState>,
    body: web::Json<CreatePaymentRequest>,
) -> HttpResponse {
    let id = Uuid::new_v4().to_string();
    let transaction_ref: String = {
        let mut rng = rand::thread_rng();
        (0..32)
            .map(|_| {
                let idx = rng.gen_range(0..36);
                if idx < 10 {
                    (b'0' + idx) as char
                } else {
                    (b'A' + idx - 10) as char
                }
            })
            .collect()
    };

    let status = "completed";

    let metadata = serde_json::json!({
        "processor": "demo-processor",
    });

    let now = Utc::now().naive_utc();

    match db_insert_payment(&data.pool, &id, &body, status, &transaction_ref, &metadata, now).await {
        Ok(_) => {
            let payment = serde_json::json!({
                "id": id,
                "order_id": body.order_id,
                "user_id": body.user_id,
                "amount": body.amount.to_string(),
                "currency": body.currency,
                "method": body.method,
                "status": status,
                "transaction_ref": transaction_ref,
                "created_at": now,
            });
            HttpResponse::Created().json(payment)
        }
        Err(e) => {
            tracing::error!("process_payment error: {}", e);
            HttpResponse::InternalServerError()
                .json(serde_json::json!({"error": e.to_string()}))
        }
    }
}

async fn get_user_payments(
    data: web::Data<AppState>,
    path: web::Path<String>,
) -> HttpResponse {
    let user_id = path.into_inner();
    match db_get_user_payments(&data.pool, &user_id).await {
        Ok(payments) => HttpResponse::Ok().json(serde_json::json!({
            "user_id": user_id,
            "payments": payments,
        })),
        Err(e) => {
            tracing::error!("get_user_payments error: {}", e);
            HttpResponse::InternalServerError()
                .json(serde_json::json!({"error": e.to_string()}))
        }
    }
}

async fn refund_payment(data: web::Data<AppState>, path: web::Path<String>) -> HttpResponse {
    let id = path.into_inner();

    match db_refund_payment(&data.pool, &id).await {
        Ok(rows_affected) => {
            if rows_affected == 0 {
                HttpResponse::BadRequest()
                    .json(serde_json::json!({"error": "payment not found or not refundable"}))
            } else {
                HttpResponse::Ok().json(serde_json::json!({"message": "payment refunded", "id": id}))
            }
        }
        Err(e) => {
            tracing::error!("refund_payment error: {}", e);
            HttpResponse::InternalServerError()
                .json(serde_json::json!({"error": e.to_string()}))
        }
    }
}

#[tracing::instrument(skip(pool))]
async fn init_db(pool: &MySqlPool) {
    sqlx::query(
        "CREATE TABLE IF NOT EXISTS payments (
            id CHAR(36) PRIMARY KEY,
            order_id VARCHAR(100),
            user_id VARCHAR(100),
            amount DECIMAL(10,2),
            currency VARCHAR(10),
            method VARCHAR(50),
            status VARCHAR(20) DEFAULT 'pending',
            transaction_ref VARCHAR(200),
            metadata JSON,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
        )"
    )
    .execute(pool)
    .await
    .expect("Failed to create payments table");

    // Migrate TIMESTAMP -> DATETIME (sqlx MySQL maps NaiveDateTime to DATETIME, not TIMESTAMP)
    let _ = sqlx::query("ALTER TABLE payments MODIFY created_at DATETIME DEFAULT CURRENT_TIMESTAMP")
        .execute(pool).await;
    let _ = sqlx::query("ALTER TABLE payments MODIFY updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP")
        .execute(pool).await;

    tracing::info!("Database initialized");
}

#[actix_web::main]
async fn main() -> std::io::Result<()> {
    let telemetry = init_telemetry();

    let database_url = env::var("DATABASE_URL").expect("DATABASE_URL must be set");

    let pool = MySqlPoolOptions::new()
        .max_connections(25)
        .acquire_timeout(Duration::from_secs(30))
        .connect(&database_url)
        .await
        .expect("Failed to create pool");

    init_db(&pool).await;

    let data = web::Data::new(AppState { pool });

    tracing::info!("Payment service listening on :8080");

    let result = HttpServer::new(move || {
        App::new()
            .app_data(data.clone())
            .wrap(TracingLogger::default())
            .wrap(RequestMetrics::default())
            .route("/health", web::get().to(health))
            .route("/payments", web::get().to(list_payments))
            .route("/payments", web::post().to(process_payment))
            .route("/payments/{id}", web::get().to(get_payment))
            .route("/payments/{id}/refund", web::post().to(refund_payment))
            .route("/payments/user/{user_id}", web::get().to(get_user_payments))
    })
    .bind("0.0.0.0:8080")?
    .run()
    .await;

    telemetry.shutdown();

    result
}
