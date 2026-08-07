"""Cart Service - Valkey-backed shopping cart."""
import os
import time
import logging

import orjson
import redis
import requests
from flask import Flask, request, Response

app = Flask(__name__)
logging.basicConfig(level=logging.INFO)


def json_response(data, status=200):
    return Response(orjson.dumps(data), status=status, content_type="application/json")

REDIS_URL = os.getenv("REDIS_URL") or os.getenv("VALKEY_URL") or "redis://valkey:6379/0"
ORDER_SERVICE_URL = os.getenv("ORDER_SERVICE_URL", "http://order-service:8080")

pool = redis.ConnectionPool.from_url(REDIS_URL, decode_responses=True, socket_timeout=5, socket_connect_timeout=3, max_connections=32)
r = redis.Redis(connection_pool=pool)


@app.route("/health")
def health():
    try:
        r.ping()
        return json_response({"status": "ok"})
    except Exception as e:
        return json_response({"status": "error", "message": str(e)}, 503)


MAX_CART_ITEMS = 100


@app.route("/cart/<user_id>", methods=["GET"])
def get_cart(user_id):
    cart_key = f"cart:{user_id}"
    cart_items = []
    cursor = 0
    while len(cart_items) < MAX_CART_ITEMS:
        cursor, items = r.hscan(cart_key, cursor=cursor, count=MAX_CART_ITEMS)
        for product_id, item_json in items.items():
            cart_items.append(orjson.loads(item_json))
            if len(cart_items) >= MAX_CART_ITEMS:
                break
        if cursor == 0:
            break
    return json_response({"user_id": user_id, "items": cart_items, "count": len(cart_items)})


@app.route("/cart/<user_id>/items", methods=["POST"])
def add_item(user_id):
    data = request.get_json()
    if not data or "product_id" not in data:
        return json_response({"error": "product_id is required"}, 400)

    cart_key = f"cart:{user_id}"
    product_id = str(data["product_id"])

    existing = r.hget(cart_key, product_id)
    if existing:
        item = orjson.loads(existing)
        item["quantity"] = item.get("quantity", 0) + data.get("quantity", 1)
    else:
        cart_size = r.hlen(cart_key)
        if cart_size >= MAX_CART_ITEMS:
            return json_response({"error": "cart is full", "max_items": MAX_CART_ITEMS}, 400)
        item = {
            "product_id": product_id,
            "name": data.get("name", ""),
            "quantity": data.get("quantity", 1),
            "price": data.get("price", 0),
        }

    pipe = r.pipeline(transaction=False)
    pipe.hset(cart_key, product_id, orjson.dumps(item))
    pipe.expire(cart_key, 604800)  # 7 days TTL
    pipe.zadd("cart:updated", {user_id: time.time()})
    pipe.execute()

    return json_response({"message": "item added", "item": item}, 201)


@app.route("/cart/<user_id>/items/<product_id>", methods=["DELETE"])
def remove_item(user_id, product_id):
    cart_key = f"cart:{user_id}"
    removed = r.hdel(cart_key, product_id)
    if removed:
        r.zadd("cart:updated", {user_id: time.time()})
        return json_response({"message": "item removed"})

    return json_response({"error": "item not found"}, 404)


@app.route("/cart/<user_id>", methods=["DELETE"])
def clear_cart(user_id):
    cart_key = f"cart:{user_id}"
    pipe = r.pipeline(transaction=False)
    pipe.delete(cart_key)
    pipe.zrem("cart:updated", user_id)
    pipe.execute()
    return json_response({"message": "cart cleared"})


@app.route("/cart/<user_id>/checkout", methods=["POST"])
def checkout(user_id):
    cart_key = f"cart:{user_id}"
    cart_items = []
    total = 0.0
    cursor = 0
    while True:
        cursor, items = r.hscan(cart_key, cursor=cursor, count=100)
        for product_id, item_json in items.items():
            item = orjson.loads(item_json)
            cart_items.append(item)
            total += item.get("price", 0) * item.get("quantity", 1)
        if cursor == 0:
            break

    if not cart_items:
        return json_response({"error": "cart is empty"}, 400)

    req_data = request.get_json()
    order_data = {
        "user_id": user_id,
        "items": [
            {
                "product_id": item["product_id"],
                "name": item.get("name", ""),
                "quantity": item.get("quantity", 1),
                "price": item.get("price", 0),
            }
            for item in cart_items
        ],
        "total": round(total, 2),
        "shipping_address": req_data.get("shipping_address", "N/A") if req_data else "N/A",
    }

    try:
        resp = requests.post(f"{ORDER_SERVICE_URL}/orders", json=order_data, timeout=10)
        if resp.status_code in (200, 201):
            r.delete(cart_key)
            r.zrem("cart:updated", user_id)
            return json_response({"message": "checkout successful", "order": resp.json()}, 201)
        return json_response({"error": "order creation failed", "details": resp.text}, resp.status_code)
    except requests.RequestException as e:
        return json_response({"error": f"order service unavailable: {e}"}, 503)


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080)
