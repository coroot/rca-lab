import asyncio
import logging
import os
import time

import grpc
import httpx
from fastapi import FastAPI, Request, Response

import recommendation_pb2
import recommendation_pb2_grpc

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("api-gateway")

PRODUCT_CATALOG_URL = os.getenv("PRODUCT_CATALOG_URL", "http://product-catalog:8080")
CART_SERVICE_URL = os.getenv("CART_SERVICE_URL", "http://cart-service:8080")
ORDER_SERVICE_URL = os.getenv("ORDER_SERVICE_URL", "http://order-service:8080")
REVIEW_SERVICE_URL = os.getenv("REVIEW_SERVICE_URL", "http://review-service:8080")
PAYMENT_SERVICE_URL = os.getenv("PAYMENT_SERVICE_URL", "http://payment-service:8080")
INVENTORY_SERVICE_URL = os.getenv("INVENTORY_SERVICE_URL", "http://inventory-service:8080")
RECOMMENDATION_SERVICE_ADDR = os.getenv("RECOMMENDATION_SERVICE_ADDR", "recommendation-service:9000")

DOWNSTREAM_SERVICES = {
    "product-catalog": PRODUCT_CATALOG_URL,
    "cart-service": CART_SERVICE_URL,
    "order-service": ORDER_SERVICE_URL,
    "review-service": REVIEW_SERVICE_URL,
    "payment-service": PAYMENT_SERVICE_URL,
    "inventory-service": INVENTORY_SERVICE_URL,
}

app = FastAPI(title="API Gateway")

# Per-upstream clients avoid O(n) connection pool scans in httpcore
upstream_clients: dict[str, httpx.AsyncClient] = {}

# gRPC channel to recommendation-service (HTTP/2)
recommendation_channel: grpc.aio.Channel | None = None
recommendation_stub: recommendation_pb2_grpc.RecommendationServiceStub | None = None


def _make_client() -> httpx.AsyncClient:
    return httpx.AsyncClient(
        limits=httpx.Limits(
            max_connections=20,
            max_keepalive_connections=20,
            keepalive_expiry=30,
        ),
        timeout=httpx.Timeout(30.0),
    )


@app.on_event("startup")
async def startup():
    for name, url in DOWNSTREAM_SERVICES.items():
        upstream_clients[url] = _make_client()
    global recommendation_channel, recommendation_stub
    recommendation_channel = grpc.aio.insecure_channel(RECOMMENDATION_SERVICE_ADDR)
    recommendation_stub = recommendation_pb2_grpc.RecommendationServiceStub(recommendation_channel)


@app.on_event("shutdown")
async def shutdown():
    for client in upstream_clients.values():
        await client.aclose()
    if recommendation_channel is not None:
        await recommendation_channel.close()


@app.middleware("http")
async def request_timing_middleware(request: Request, call_next):
    start = time.perf_counter()
    response = await call_next(request)
    elapsed_ms = (time.perf_counter() - start) * 1000
    logger.info("%s %s completed in %.2f ms (status %d)",
                request.method, request.url.path, elapsed_ms, response.status_code)
    response.headers["X-Response-Time-Ms"] = f"{elapsed_ms:.2f}"
    return response


@app.get("/health")
async def health():
    async def check(name: str, url: str):
        try:
            client = upstream_clients.get(url, next(iter(upstream_clients.values())))
            resp = await client.get(f"{url}/health", timeout=3.0)
            return name, {"status": "healthy", "status_code": resp.status_code}
        except httpx.RequestError as exc:
            return name, {"status": "unhealthy", "error": str(exc)}

    checks = await asyncio.gather(
        *(check(name, url) for name, url in DOWNSTREAM_SERVICES.items())
    )
    results = dict(checks)
    all_healthy = all(s.get("status") == "healthy" for s in results.values())
    return {
        "status": "healthy" if all_healthy else "degraded",
        "services": results,
    }


async def _proxy(request: Request, upstream_url: str, downstream_path: str) -> Response:
    url = f"{upstream_url}{downstream_path}"
    if request.url.query:
        url = f"{url}?{request.url.query}"

    body = await request.body()

    headers = dict(request.headers)
    headers.pop("host", None)

    client = upstream_clients[upstream_url]

    try:
        resp = await client.request(
            method=request.method,
            url=url,
            headers=headers,
            content=body,
        )
    except httpx.ConnectError as exc:
        logger.error("Upstream connect error: %s %s -> %s", request.method, downstream_path, exc)
        return Response(
            content='{"error": "Service unavailable"}',
            status_code=503,
            media_type="application/json",
        )
    except httpx.TimeoutException as exc:
        logger.error("Upstream timeout: %s %s -> %s", request.method, downstream_path, exc)
        return Response(
            content='{"error": "Service timeout"}',
            status_code=504,
            media_type="application/json",
        )
    except httpx.RequestError as exc:
        logger.error("Upstream request failed: %s %s -> %s", request.method, downstream_path, exc)
        return Response(
            content=f'{{"error": "Upstream request failed: {exc}"}}',
            status_code=502,
            media_type="application/json",
        )

    excluded_headers = {"content-encoding", "content-length", "transfer-encoding"}
    response_headers = {
        k: v for k, v in resp.headers.items() if k.lower() not in excluded_headers
    }

    return Response(
        content=resp.content,
        status_code=resp.status_code,
        headers=response_headers,
        media_type=resp.headers.get("content-type"),
    )


@app.api_route("/api/products/{path:path}", methods=["GET", "POST"])
@app.api_route("/api/products", methods=["GET", "POST"])
async def proxy_products(request: Request, path: str = ""):
    return await _proxy(request, PRODUCT_CATALOG_URL, f"/products/{path}" if path else "/products")


@app.api_route("/api/cart/{path:path}", methods=["GET", "POST", "DELETE"])
@app.api_route("/api/cart", methods=["GET", "POST", "DELETE"])
async def proxy_cart(request: Request, path: str = ""):
    return await _proxy(request, CART_SERVICE_URL, f"/cart/{path}" if path else "/cart")


@app.api_route("/api/orders/{path:path}", methods=["GET", "POST", "PUT"])
@app.api_route("/api/orders", methods=["GET", "POST", "PUT"])
async def proxy_orders(request: Request, path: str = ""):
    return await _proxy(request, ORDER_SERVICE_URL, f"/orders/{path}" if path else "/orders")


@app.api_route("/api/reviews/{path:path}", methods=["GET", "POST"])
@app.api_route("/api/reviews", methods=["GET", "POST"])
async def proxy_reviews(request: Request, path: str = ""):
    return await _proxy(request, REVIEW_SERVICE_URL, f"/reviews/{path}" if path else "/reviews")


@app.api_route("/api/payments/{path:path}", methods=["GET", "POST"])
@app.api_route("/api/payments", methods=["GET", "POST"])
async def proxy_payments(request: Request, path: str = ""):
    return await _proxy(request, PAYMENT_SERVICE_URL, f"/payments/{path}" if path else "/payments")


@app.api_route("/api/inventory/{path:path}", methods=["GET", "POST", "PUT"])
@app.api_route("/api/inventory", methods=["GET", "POST", "PUT"])
async def proxy_inventory(request: Request, path: str = ""):
    return await _proxy(request, INVENTORY_SERVICE_URL, f"/inventory/{path}" if path else "/inventory")


@app.get("/api/recommendations/{user_id}")
async def recommendations(user_id: str, limit: int = 10):
    if recommendation_stub is None:
        return Response(content='{"error": "recommendation service not initialized"}',
                        status_code=503, media_type="application/json")
    try:
        req = recommendation_pb2.GetRecommendationsRequest(user_id=user_id, limit=limit)
        resp = await recommendation_stub.GetRecommendations(req, timeout=3.0)
        return {"user_id": user_id, "product_ids": list(resp.product_ids)}
    except grpc.aio.AioRpcError as exc:
        logger.error("recommendation gRPC call failed: %s", exc)
        return Response(content=f'{{"error": "recommendation upstream: {exc.code().name}"}}',
                        status_code=502, media_type="application/json")


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8080)
