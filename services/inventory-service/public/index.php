<?php

use OpenTelemetry\API\Globals;
use OpenTelemetry\API\Trace\SpanKind;

require_once __DIR__ . '/../vendor/autoload.php';
require_once __DIR__ . '/../src/Database.php';
require_once __DIR__ . '/../src/Logger.php';

header('Content-Type: application/json');

$method = $_SERVER['REQUEST_METHOD'];
$uri = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
$uri = rtrim($uri, '/');

// --- OpenTelemetry root span ---
// Auto-instrumentation covers PDO, but plain (non-framework) PHP has no
// incoming-request instrumentation, so create the server span manually.
$otelHeaders = array_change_key_case(function_exists('getallheaders') ? (getallheaders() ?: []) : [], CASE_LOWER);
$otelSpan = Globals::tracerProvider()->getTracer('inventory-service')
    ->spanBuilder($method . ' ' . ($uri ?: '/'))
    ->setSpanKind(SpanKind::KIND_SERVER)
    ->setParent(Globals::propagator()->extract($otelHeaders))
    ->setAttribute('http.request.method', $method)
    ->setAttribute('url.path', $uri ?: '/')
    ->startSpan();
$otelScope = $otelSpan->activate();
register_shutdown_function(function () use ($otelSpan, $otelScope): void {
    $otelSpan->setAttribute('http.response.status_code', http_response_code());
    $otelScope->detach();
    $otelSpan->end();
});

// --- Auto-migration ---
function migrate(): void
{
    $db = Database::get();
    $db->exec("
        CREATE TABLE IF NOT EXISTS inventory_stock (
            product_id INTEGER PRIMARY KEY,
            quantity INTEGER NOT NULL DEFAULT 0,
            reserved INTEGER NOT NULL DEFAULT 0,
            warehouse VARCHAR(64) NOT NULL DEFAULT 'main',
            updated_at TIMESTAMP NOT NULL DEFAULT NOW()
        );
        CREATE TABLE IF NOT EXISTS inventory_reservations (
            id SERIAL PRIMARY KEY,
            product_id INTEGER NOT NULL REFERENCES inventory_stock(product_id),
            user_id VARCHAR(64) NOT NULL,
            quantity INTEGER NOT NULL,
            status VARCHAR(16) NOT NULL DEFAULT 'active',
            created_at TIMESTAMP NOT NULL DEFAULT NOW(),
            expires_at TIMESTAMP NOT NULL DEFAULT (NOW() + INTERVAL '15 minutes')
        );
        CREATE INDEX IF NOT EXISTS idx_stock_quantity ON inventory_stock (quantity);
        CREATE INDEX IF NOT EXISTS idx_reservations_product ON inventory_reservations (product_id, status);
        CREATE INDEX IF NOT EXISTS idx_reservations_user ON inventory_reservations (user_id, status);
    ");
}

// Run migration once per container (marker file persists across requests)
$migrationMarker = '/tmp/.inventory_migrated';
if (!file_exists($migrationMarker)) {
    try {
        migrate();
        file_put_contents($migrationMarker, date('c'));
    } catch (Exception $e) {
        // Tables likely already exist
    }
}

// --- Routing ---
function jsonOut($data, int $code = 200): void
{
    http_response_code($code);
    echo json_encode($data);
    exit;
}

function getInput(): array
{
    return json_decode(file_get_contents('php://input'), true) ?? [];
}

try {
    $db = Database::get();

    // Health
    if ($uri === '/health') {
        $db->query('SELECT 1');
        jsonOut(['status' => 'healthy', 'service' => 'inventory-service']);
    }

    // GET /inventory/low-stock
    if ($uri === '/inventory/low-stock' && $method === 'GET') {
        $threshold = (int) ($_GET['threshold'] ?? 10);
        $stmt = $db->prepare('SELECT * FROM inventory_stock WHERE quantity < :t ORDER BY quantity ASC LIMIT 50');
        $stmt->execute([':t' => $threshold]);
        jsonOut($stmt->fetchAll());
    }

    // POST /inventory/reserve
    if ($uri === '/inventory/reserve' && $method === 'POST') {
        $input = getInput();
        $productId = (int) ($input['product_id'] ?? 0);
        $userId = $input['user_id'] ?? '';
        $qty = (int) ($input['quantity'] ?? 1);

        if (!$productId || !$userId) {
            jsonOut(['error' => 'product_id and user_id required'], 400);
        }

        $db->beginTransaction();
        $stmt = $db->prepare('SELECT quantity, reserved FROM inventory_stock WHERE product_id = :id FOR UPDATE');
        $stmt->execute([':id' => $productId]);
        $stock = $stmt->fetch();

        if (!$stock) {
            $db->rollBack();
            jsonOut(['error' => 'Product not found'], 404);
        }

        $available = $stock['quantity'] - $stock['reserved'];
        if ($available < $qty) {
            $db->rollBack();
            jsonOut(['error' => 'Insufficient stock', 'available' => $available], 409);
        }

        $stmt = $db->prepare('UPDATE inventory_stock SET reserved = reserved + :qty, updated_at = NOW() WHERE product_id = :id');
        $stmt->execute([':qty' => $qty, ':id' => $productId]);

        $stmt = $db->prepare('INSERT INTO inventory_reservations (product_id, user_id, quantity) VALUES (:pid, :uid, :qty) RETURNING id');
        $stmt->execute([':pid' => $productId, ':uid' => $userId, ':qty' => $qty]);
        $resId = $stmt->fetchColumn();

        $db->commit();
        jsonOut(['reservation_id' => $resId, 'product_id' => $productId, 'quantity' => $qty], 201);
    }

    // POST /inventory/release/{id}
    if (preg_match('#^/inventory/release/(\d+)$#', $uri, $m) && $method === 'POST') {
        $resId = (int) $m[1];

        $db->beginTransaction();
        $stmt = $db->prepare('SELECT * FROM inventory_reservations WHERE id = :id AND status = \'active\' FOR UPDATE');
        $stmt->execute([':id' => $resId]);
        $res = $stmt->fetch();

        if (!$res) {
            $db->rollBack();
            jsonOut(['error' => 'Reservation not found or already released'], 404);
        }

        $stmt = $db->prepare('UPDATE inventory_stock SET reserved = GREATEST(reserved - :qty, 0), updated_at = NOW() WHERE product_id = :pid');
        $stmt->execute([':qty' => $res['quantity'], ':pid' => $res['product_id']]);

        $stmt = $db->prepare('UPDATE inventory_reservations SET status = \'released\' WHERE id = :id');
        $stmt->execute([':id' => $resId]);

        $db->commit();
        jsonOut(['status' => 'released', 'reservation_id' => $resId]);
    }

    // PUT /inventory/{sku}/reserve — atomically decrement stock (called by the fulfillment service)
    if (preg_match('#^/inventory/(\d+)/reserve$#', $uri, $m) && $method === 'PUT') {
        $id = (int) $m[1];
        $input = getInput();
        $qty = (int) ($input['quantity'] ?? 0);

        if ($qty <= 0) {
            jsonOut(['error' => 'quantity must be a positive integer'], 400);
        }

        $stmt = $db->prepare('
            UPDATE inventory_stock
            SET quantity = quantity - :qty, updated_at = NOW()
            WHERE product_id = :id AND quantity >= :qty2
            RETURNING *
        ');
        $stmt->execute([':qty' => $qty, ':id' => $id, ':qty2' => $qty]);
        $row = $stmt->fetch();

        if (!$row) {
            $stmt = $db->prepare('SELECT quantity FROM inventory_stock WHERE product_id = :id');
            $stmt->execute([':id' => $id]);
            $current = $stmt->fetchColumn();
            if ($current === false) {
                jsonOut(['error' => 'Product not found'], 404);
            }
            jsonOut(['error' => 'Insufficient stock', 'available' => (int) $current], 409);
        }

        jsonOut($row);
    }

    // GET /inventory/{id}
    if (preg_match('#^/inventory/(\d+)$#', $uri, $m) && $method === 'GET') {
        $id = (int) $m[1];
        $stmt = $db->prepare('SELECT * FROM inventory_stock WHERE product_id = :id');
        $stmt->execute([':id' => $id]);
        $row = $stmt->fetch();
        if (!$row) {
            jsonOut(['product_id' => $id, 'quantity' => 0, 'reserved' => 0, 'warehouse' => 'none', 'updated_at' => null]);
        }
        jsonOut($row);
    }

    // PUT /inventory/{id}
    if (preg_match('#^/inventory/(\d+)$#', $uri, $m) && $method === 'PUT') {
        $id = (int) $m[1];
        $input = getInput();
        $qty = (int) ($input['quantity'] ?? 0);
        $warehouse = $input['warehouse'] ?? 'main';

        $stmt = $db->prepare('
            INSERT INTO inventory_stock (product_id, quantity, warehouse, updated_at)
            VALUES (:id, :qty, :wh, NOW())
            ON CONFLICT (product_id) DO UPDATE SET quantity = :qty2, warehouse = :wh2, updated_at = NOW()
            RETURNING *
        ');
        $stmt->execute([':id' => $id, ':qty' => $qty, ':wh' => $warehouse, ':qty2' => $qty, ':wh2' => $warehouse]);
        jsonOut($stmt->fetch());
    }

    // GET /inventory (paginated list)
    if ($uri === '/inventory' && $method === 'GET') {
        $page = max(1, (int) ($_GET['page'] ?? 1));
        $limit = min(100, max(1, (int) ($_GET['limit'] ?? 20)));
        $offset = ($page - 1) * $limit;

        $stmt = $db->prepare('SELECT * FROM inventory_stock ORDER BY product_id LIMIT :lim OFFSET :off');
        $stmt->bindValue(':lim', $limit, PDO::PARAM_INT);
        $stmt->bindValue(':off', $offset, PDO::PARAM_INT);
        $stmt->execute();

        $count = $db->query("SELECT reltuples::bigint FROM pg_class WHERE relname = 'inventory_stock'")->fetchColumn();

        jsonOut([
            'items' => $stmt->fetchAll(),
            'page' => $page,
            'limit' => $limit,
            'total' => (int) $count,
        ]);
    }

    // 404 fallback
    jsonOut(['error' => 'Not found'], 404);

} catch (PDOException $e) {
    Logger::get()->error('Database error: ' . $e->getMessage());
    jsonOut(['error' => 'Database error'], 500);
} catch (Exception $e) {
    Logger::get()->error('Error: ' . $e->getMessage());
    jsonOut(['error' => 'Internal server error'], 500);
}
