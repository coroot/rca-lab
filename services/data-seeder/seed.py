#!/usr/bin/env python3
"""
Data seeder for rca-lab databases.
Seeds ~10GB of data into each database.
"""

import os
import sys
import json
import time
import random
import string
import logging
import uuid
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta

from faker import Faker

logging.basicConfig(level=logging.INFO, format='%(asctime)s %(levelname)s %(message)s')
log = logging.getLogger(__name__)
fake = Faker()

# Config
PG_PRODUCTS_URL = os.getenv("PG_PRODUCTS_URL", "host=postgres-products dbname=products user=postgres")
PG_INVENTORY_URL = os.getenv("PG_INVENTORY_URL", "host=postgres-inventory dbname=inventory user=postgres")
MYSQL_HOST = os.getenv("MYSQL_HOST", "mysql-haproxy")
MYSQL_PORT = int(os.getenv("MYSQL_PORT", "3306"))
MYSQL_ORDERS_USER = os.getenv("MYSQL_ORDERS_USER", "orders")
MYSQL_ORDERS_PASSWORD = os.getenv("MYSQL_ORDERS_PASSWORD", "")
MYSQL_ORDERS_DB = "orders"
MYSQL_PAYMENTS_USER = os.getenv("MYSQL_PAYMENTS_USER", "payments")
MYSQL_PAYMENTS_PASSWORD = os.getenv("MYSQL_PAYMENTS_PASSWORD", "")
MYSQL_PAYMENTS_DB = "payments"
MONGO_URI = os.getenv("MONGO_URI", "mongodb://mongo:27017/reviews")
VALKEY_URL = os.getenv("VALKEY_URL", "redis://valkey:6379/0")

BATCH_SIZE = int(os.getenv("BATCH_SIZE", "5000"))
TARGET_SIZE_GB = float(os.getenv("TARGET_SIZE_GB", "10"))


def generate_large_text(min_len=500, max_len=2000):
    return fake.text(max_nb_chars=random.randint(min_len, max_len))


def generate_metadata():
    return json.dumps({
        "weight": round(random.uniform(0.1, 50.0), 2),
        "dimensions": f"{random.randint(1,100)}x{random.randint(1,100)}x{random.randint(1,100)}",
        "color": fake.color_name(),
        "material": random.choice(["plastic", "metal", "wood", "fabric", "glass", "ceramic", "leather"]),
        "brand": fake.company(),
        "warranty_months": random.choice([0, 6, 12, 24, 36]),
        "tags": [fake.word() for _ in range(random.randint(2, 8))],
        "specifications": {fake.word(): fake.word() for _ in range(random.randint(3, 10))},
    })


CATEGORIES = ["electronics", "clothing", "books", "home", "sports", "toys", "food", "beauty", "automotive", "garden"]


def wait_for_connection(connect_fn, name, max_retries=30):
    for i in range(max_retries):
        try:
            conn = connect_fn()
            log.info(f"Connected to {name}")
            return conn
        except Exception as e:
            log.warning(f"Waiting for {name}... ({i+1}/{max_retries}): {e}")
            time.sleep(10)
    log.error(f"Failed to connect to {name}")
    sys.exit(1)


def seed_postgres_products():
    import psycopg2
    conn = wait_for_connection(lambda: psycopg2.connect(PG_PRODUCTS_URL), "PostgreSQL (products)")
    conn.autocommit = True
    cur = conn.cursor()

    cur.execute("""
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
    """)

    cur.execute("SELECT COUNT(*) FROM products")
    existing = cur.fetchone()[0]
    log.info(f"PostgreSQL products: {existing} existing rows")

    # Estimate: each row ~2KB, need ~5M rows for 10GB
    target_rows = int(TARGET_SIZE_GB * 1024 * 1024 * 1024 / 2048)
    remaining = target_rows - existing

    if remaining <= 0:
        log.info("PostgreSQL products already seeded")
        conn.close()
        return

    log.info(f"Seeding {remaining} product rows (~{TARGET_SIZE_GB}GB)")
    inserted = 0
    conn.autocommit = False

    while inserted < remaining:
        batch = min(BATCH_SIZE, remaining - inserted)
        values = []
        for _ in range(batch):
            name = f"{fake.word().title()} {fake.word().title()} {random.choice(['Pro', 'Plus', 'Max', 'Lite', 'Ultra', 'Mini', 'XL'])}"
            desc = generate_large_text(800, 2000)
            cat = random.choice(CATEGORIES)
            price = round(random.uniform(0.99, 999.99), 2)
            sku = ''.join(random.choices(string.ascii_uppercase + string.digits, k=12))
            metadata = generate_metadata()
            image_url = f"https://images.example.com/products/{sku.lower()}.jpg"
            values.append((name, desc, cat, price, sku, metadata, image_url))

        args_str = ",".join(
            cur.mogrify("(%s,%s,%s,%s,%s,%s,%s)", v).decode() for v in values
        )
        cur.execute(f"INSERT INTO products (name,description,category,price,sku,metadata,image_url) VALUES {args_str}")
        conn.commit()
        inserted += batch

        if inserted % 50000 == 0:
            log.info(f"PostgreSQL products: {inserted}/{remaining} rows inserted")

    # Create indexes (only indexes actually used by queries)
    cur.execute("CREATE EXTENSION IF NOT EXISTS pg_trgm")
    cur.execute("CREATE INDEX IF NOT EXISTS idx_products_name_trgm ON products USING gin (name gin_trgm_ops)")
    conn.commit()
    conn.close()
    log.info(f"PostgreSQL products seeding complete: {inserted} rows")


def count_postgres_products():
    """Count rows in the products database (separate cluster from inventory)."""
    import psycopg2
    conn = wait_for_connection(lambda: psycopg2.connect(PG_PRODUCTS_URL), "PostgreSQL (products, for count)")
    cur = conn.cursor()
    cur.execute("SELECT COUNT(*) FROM products")
    total = cur.fetchone()[0]
    conn.close()
    return total


def seed_postgres_inventory():
    import psycopg2
    conn = wait_for_connection(lambda: psycopg2.connect(PG_INVENTORY_URL), "PostgreSQL (inventory)")
    conn.autocommit = True
    cur = conn.cursor()

    cur.execute("""
        CREATE TABLE IF NOT EXISTS inventory_stock (
            product_id INTEGER PRIMARY KEY,
            quantity INTEGER NOT NULL DEFAULT 0,
            reserved INTEGER NOT NULL DEFAULT 0,
            warehouse VARCHAR(64) NOT NULL DEFAULT 'main',
            updated_at TIMESTAMP NOT NULL DEFAULT NOW()
        )
    """)
    cur.execute("""
        CREATE TABLE IF NOT EXISTS inventory_reservations (
            id SERIAL PRIMARY KEY,
            product_id INTEGER NOT NULL REFERENCES inventory_stock(product_id),
            user_id VARCHAR(64) NOT NULL,
            quantity INTEGER NOT NULL,
            status VARCHAR(16) NOT NULL DEFAULT 'active',
            created_at TIMESTAMP NOT NULL DEFAULT NOW(),
            expires_at TIMESTAMP NOT NULL DEFAULT (NOW() + INTERVAL '15 minutes')
        )
    """)
    cur.execute("CREATE INDEX IF NOT EXISTS idx_stock_quantity ON inventory_stock (quantity)")
    cur.execute("CREATE INDEX IF NOT EXISTS idx_reservations_product ON inventory_reservations (product_id, status)")
    cur.execute("CREATE INDEX IF NOT EXISTS idx_reservations_user ON inventory_reservations (user_id, status)")

    cur.execute("SELECT COUNT(*) FROM inventory_stock")
    existing = cur.fetchone()[0]
    log.info(f"PostgreSQL inventory_stock: {existing} existing rows")

    # Match product IDs from the products database
    total_products = count_postgres_products()

    remaining = total_products - existing
    if remaining <= 0:
        log.info("PostgreSQL inventory already seeded")
        conn.close()
        return

    log.info(f"Seeding {remaining} inventory rows for {total_products} products")
    conn.autocommit = False

    warehouses = ["main", "east", "west", "central", "south"]
    offset = existing
    inserted = 0

    while inserted < remaining:
        batch = min(BATCH_SIZE, remaining - inserted)
        values = []
        for i in range(batch):
            product_id = offset + inserted + i + 1
            # ~10% of products are low stock (0-9), rest have 10-500
            if random.random() < 0.10:
                qty = random.randint(0, 9)
            else:
                qty = random.randint(10, 500)
            reserved = random.randint(0, min(qty, 20))
            warehouse = random.choice(warehouses)
            values.append((product_id, qty, reserved, warehouse))

        args_str = ",".join(
            cur.mogrify("(%s,%s,%s,%s,NOW())", v).decode() for v in values
        )
        cur.execute(f"INSERT INTO inventory_stock (product_id,quantity,reserved,warehouse,updated_at) VALUES {args_str} ON CONFLICT DO NOTHING")
        conn.commit()
        inserted += batch

        if inserted % 50000 == 0:
            log.info(f"PostgreSQL inventory: {inserted}/{remaining} rows inserted")

    conn.close()
    log.info(f"PostgreSQL inventory seeding complete: {inserted} rows")


def seed_mysql_payments():
    import mysql.connector
    conn = wait_for_connection(
        lambda: mysql.connector.connect(host=MYSQL_HOST, port=MYSQL_PORT, user=MYSQL_PAYMENTS_USER, password=MYSQL_PAYMENTS_PASSWORD, database=MYSQL_PAYMENTS_DB),
        "MySQL (payments)"
    )
    cur = conn.cursor()

    cur.execute("""
        CREATE TABLE IF NOT EXISTS payments (
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
        )
    """)
    conn.commit()

    cur.execute("SELECT COUNT(*) FROM payments")
    existing = cur.fetchone()[0]
    log.info(f"MySQL payments: {existing} existing rows")

    # Each row ~500 bytes (UUID + varchars + decimal + json + timestamps)
    target_rows = int(TARGET_SIZE_GB * 1024 * 1024 * 1024 / 512)
    remaining = target_rows - existing

    if remaining <= 0:
        log.info("MySQL payments already seeded")
        conn.close()
        return

    log.info(f"Seeding {remaining} payment rows (~{TARGET_SIZE_GB}GB)")
    inserted = 0
    statuses = ["completed", "pending", "failed", "refunded"]
    methods = ["card", "paypal", "crypto"]
    currencies = ["USD", "EUR", "GBP", "JPY", "CAD"]

    while inserted < remaining:
        batch = min(BATCH_SIZE, remaining - inserted)
        values = []
        for _ in range(batch):
            uid = str(uuid.uuid4())
            order_id = f"order-{random.randint(1, 5000000)}"
            user_id = f"user-{random.randint(1, 100000)}"
            amount = round(random.uniform(1.0, 999.99), 2)
            currency = random.choice(currencies)
            method = random.choice(methods)
            status = random.choice(statuses)
            tx_ref = ''.join(random.choices(string.ascii_uppercase + string.digits, k=32))
            metadata = json.dumps({
                "ip": fake.ipv4(),
                "user_agent": fake.user_agent(),
                "card_last4": f"{random.randint(1000,9999)}",
                "processor_response": random.choice(["approved", "declined", "error", "timeout"]),
            })
            days_ago = random.randint(0, 365)
            created = datetime.now() - timedelta(days=days_ago, hours=random.randint(0, 23))
            values.append((uid, order_id, user_id, amount, currency, method, status, tx_ref, metadata, created, created))

        cur.executemany(
            "INSERT INTO payments (id,order_id,user_id,amount,currency,method,status,transaction_ref,metadata,created_at,updated_at) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)",
            values
        )
        conn.commit()
        inserted += batch

        if inserted % 50000 == 0:
            log.info(f"MySQL payments: {inserted}/{remaining} rows inserted")

    # Create indexes (only indexes actually used by queries)
    for stmt in [
        "CREATE INDEX idx_payments_user_id ON payments(user_id)",
        "CREATE INDEX idx_payments_created_at ON payments(created_at DESC)",
    ]:
        try:
            cur.execute(stmt)
        except Exception:
            pass
    conn.commit()
    conn.close()
    log.info(f"MySQL payments seeding complete: {inserted} rows")


def seed_mysql_orders():
    import mysql.connector
    conn = wait_for_connection(
        lambda: mysql.connector.connect(host=MYSQL_HOST, port=MYSQL_PORT, user=MYSQL_ORDERS_USER, password=MYSQL_ORDERS_PASSWORD, database=MYSQL_ORDERS_DB),
        "MySQL (orders)"
    )
    cur = conn.cursor()

    cur.execute("""
        CREATE TABLE IF NOT EXISTS orders (
            id BIGINT AUTO_INCREMENT PRIMARY KEY,
            user_id VARCHAR(100),
            status ENUM('PENDING','CONFIRMED','SHIPPED','DELIVERED','CANCELLED') DEFAULT 'PENDING',
            total DECIMAL(10,2),
            shipping_address VARCHAR(500),
            payment_id VARCHAR(200),
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
        )
    """)
    cur.execute("""
        CREATE TABLE IF NOT EXISTS order_items (
            id BIGINT AUTO_INCREMENT PRIMARY KEY,
            order_id BIGINT,
            product_id VARCHAR(100),
            name VARCHAR(500),
            quantity INT,
            price DECIMAL(10,2),
            FOREIGN KEY (order_id) REFERENCES orders(id)
        )
    """)
    cur.execute("""
        CREATE TABLE IF NOT EXISTS shipments (
            id BIGINT AUTO_INCREMENT PRIMARY KEY,
            order_id BIGINT,
            status VARCHAR(32) DEFAULT 'PENDING',
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        )
    """)
    conn.commit()

    cur.execute("SELECT COUNT(*) FROM orders")
    existing = cur.fetchone()[0]

    # Each order ~500 bytes + ~3 items at ~200 bytes each = ~1.1KB per order
    target_rows = int(TARGET_SIZE_GB * 1024 * 1024 * 1024 / 1100)
    remaining = target_rows - existing

    if remaining <= 0:
        log.info("MySQL orders already seeded")
        conn.close()
        return

    log.info(f"Seeding {remaining} orders (~{TARGET_SIZE_GB}GB)")
    inserted = 0
    statuses = ["PENDING", "CONFIRMED", "SHIPPED", "DELIVERED", "CANCELLED"]

    while inserted < remaining:
        batch = min(BATCH_SIZE, remaining - inserted)

        order_values = []
        for _ in range(batch):
            user_id = f"user-{random.randint(1, 100000)}"
            status = random.choice(statuses)
            total = round(random.uniform(10.0, 2000.0), 2)
            address = f"{fake.street_address()}, {fake.city()}, {fake.state()} {fake.zipcode()}"
            payment_id = ''.join(random.choices(string.ascii_lowercase + string.digits, k=36))
            order_values.append((user_id, status, total, address, payment_id))

        cur.executemany(
            "INSERT INTO orders (user_id,status,total,shipping_address,payment_id) VALUES (%s,%s,%s,%s,%s)",
            order_values
        )
        conn.commit()

        # Get the range of inserted IDs
        cur.execute("SELECT LAST_INSERT_ID()")
        last_id = cur.fetchone()[0]
        first_id = last_id - batch + 1

        # Insert order items (2-4 items per order)
        item_values = []
        for i in range(batch):
            order_id = first_id + i
            num_items = random.randint(2, 4)
            for _ in range(num_items):
                product_id = f"{random.randint(1, 500000)}"
                name = f"{fake.word().title()} {fake.word().title()}"
                qty = random.randint(1, 5)
                price = round(random.uniform(5.0, 500.0), 2)
                item_values.append((order_id, product_id, name, qty, price))

            if len(item_values) >= 10000:
                cur.executemany(
                    "INSERT INTO order_items (order_id,product_id,name,quantity,price) VALUES (%s,%s,%s,%s,%s)",
                    item_values
                )
                conn.commit()
                item_values = []

        if item_values:
            cur.executemany(
                "INSERT INTO order_items (order_id,product_id,name,quantity,price) VALUES (%s,%s,%s,%s,%s)",
                item_values
            )
            conn.commit()

        inserted += batch
        if inserted % 50000 == 0:
            log.info(f"MySQL orders: {inserted}/{remaining} rows inserted")

    # Create indexes (only indexes actually used by queries)
    for stmt in [
        "CREATE INDEX idx_orders_user_created ON orders(user_id, created_at DESC)",
        "CREATE INDEX idx_order_items_order_id ON order_items(order_id)",
    ]:
        try:
            cur.execute(stmt)
        except Exception:
            pass
    conn.commit()
    conn.close()
    log.info(f"MySQL orders seeding complete: {inserted} rows")


def seed_mongodb_reviews():
    from pymongo import MongoClient
    client = wait_for_connection(lambda: MongoClient(MONGO_URI, serverSelectionTimeoutMS=5000).admin.command('ping') or MongoClient(MONGO_URI), "MongoDB")
    if not isinstance(client, MongoClient):
        client = MongoClient(MONGO_URI)

    db = client.get_default_database()
    coll = db.reviews

    existing = coll.estimated_document_count()
    log.info(f"MongoDB reviews: {existing} existing documents")

    # Each doc ~2KB, need ~5M for 10GB
    target_docs = int(TARGET_SIZE_GB * 1024 * 1024 * 1024 / 2048)
    remaining = target_docs - existing

    if remaining <= 0:
        log.info("MongoDB reviews already seeded")
        client.close()
        return

    log.info(f"Seeding {remaining} review documents (~{TARGET_SIZE_GB}GB)")
    inserted = 0

    while inserted < remaining:
        batch = min(BATCH_SIZE, remaining - inserted)
        docs = []
        for _ in range(batch):
            days_ago = random.randint(0, 730)
            created = datetime.now() - timedelta(days=days_ago)
            doc = {
                "product_id": str(random.randint(1, 500000)),
                "user_id": f"user-{random.randint(1, 100000)}",
                "rating": random.randint(1, 5),
                "title": fake.sentence(nb_words=random.randint(4, 10)),
                "body": generate_large_text(500, 1500),
                "pros": [fake.sentence(nb_words=3) for _ in range(random.randint(1, 5))],
                "cons": [fake.sentence(nb_words=3) for _ in range(random.randint(0, 3))],
                "helpful_count": random.randint(0, 500),
                "verified_purchase": random.random() > 0.3,
                "created_at": created,
                "updated_at": created,
            }
            docs.append(doc)

        coll.insert_many(docs)
        inserted += batch

        if inserted % 50000 == 0:
            log.info(f"MongoDB reviews: {inserted}/{remaining} documents inserted")

    # Create indexes (only indexes actually used by queries)
    coll.create_index([("product_id", 1), ("created_at", -1)])
    coll.create_index([("created_at", -1)])
    client.close()
    log.info(f"MongoDB reviews seeding complete: {inserted} documents")


def seed_valkey_carts():
    import redis as redis_lib
    r = wait_for_connection(lambda: redis_lib.from_url(VALKEY_URL, decode_responses=True, socket_connect_timeout=5) or True, "Valkey")
    if not isinstance(r, redis_lib.Redis):
        r = redis_lib.from_url(VALKEY_URL, decode_responses=True)

    r.ping()
    existing_keys = r.dbsize()
    log.info(f"Valkey: {existing_keys} existing keys")

    # Valkey 10GB: lots of cart data + product cache + session data
    # Each cart ~1KB with items, need ~10M keys
    target_keys = int(TARGET_SIZE_GB * 1024 * 1024 * 1024 / 1024)
    remaining = target_keys - existing_keys

    if remaining <= 0:
        log.info("Valkey already seeded")
        return

    log.info(f"Seeding {remaining} Valkey keys (~{TARGET_SIZE_GB}GB)")
    inserted = 0
    pipe = r.pipeline()

    while inserted < remaining:
        batch = min(BATCH_SIZE, remaining - inserted)
        for i in range(batch):
            key_type = random.random()
            if key_type < 0.4:
                # Cart data
                user_id = f"user-{random.randint(1, 1000000)}"
                cart_key = f"cart:{user_id}"
                num_items = random.randint(1, 8)
                for _ in range(num_items):
                    product_id = str(random.randint(1, 500000))
                    item = json.dumps({
                        "product_id": product_id,
                        "name": f"{fake.word().title()} {fake.word().title()}",
                        "quantity": random.randint(1, 5),
                        "price": round(random.uniform(5.0, 500.0), 2),
                    })
                    pipe.hset(cart_key, product_id, item)
                pipe.expire(cart_key, 604800)  # 7 days
            elif key_type < 0.7:
                # Product cache
                pid = random.randint(1, 500000)
                cache_data = json.dumps({
                    "id": pid,
                    "name": f"{fake.word().title()} Product",
                    "price": round(random.uniform(1.0, 999.0), 2),
                    "category": random.choice(CATEGORIES),
                    "description": fake.text(max_nb_chars=500),
                })
                pipe.set(f"product:cache:{pid}", cache_data, ex=3600)
            else:
                # Session data
                session_id = ''.join(random.choices(string.ascii_lowercase + string.digits, k=32))
                session_data = json.dumps({
                    "user_id": f"user-{random.randint(1, 100000)}",
                    "ip": fake.ipv4(),
                    "user_agent": fake.user_agent(),
                    "last_activity": datetime.now().isoformat(),
                    "preferences": {fake.word(): fake.word() for _ in range(5)},
                })
                pipe.set(f"session:{session_id}", session_data, ex=86400)

        pipe.execute()
        inserted += batch

        if inserted % 50000 == 0:
            log.info(f"Valkey: {inserted}/{remaining} keys inserted")

    log.info(f"Valkey seeding complete: {inserted} keys")


def main():
    target = os.getenv("SEED_TARGET", "all")
    log.info(f"Starting data seeder (target={target}, size={TARGET_SIZE_GB}GB per database)")

    if target == "all":
        with ThreadPoolExecutor(max_workers=6) as pool:
            futures = [
                pool.submit(seed_postgres_products),
                pool.submit(seed_postgres_inventory),
                pool.submit(seed_mysql_payments),
                pool.submit(seed_mysql_orders),
                pool.submit(seed_mongodb_reviews),
                pool.submit(seed_valkey_carts),
            ]
            for f in futures:
                try:
                    f.result()
                except Exception as e:
                    log.error(f"Seeder failed: {e}", exc_info=True)
    elif target == "postgres-products":
        seed_postgres_products()
    elif target == "postgres-inventory":
        seed_postgres_inventory()
    elif target == "mysql-payments":
        seed_mysql_payments()
    elif target == "mysql-orders":
        seed_mysql_orders()
    elif target == "mongodb":
        seed_mongodb_reviews()
    elif target == "valkey":
        seed_valkey_carts()
    else:
        log.error(f"Unknown target: {target}")
        sys.exit(1)

    log.info("Data seeding complete!")


if __name__ == "__main__":
    main()
