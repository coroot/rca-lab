package com.coroot.orderservice.service;

import com.coroot.orderservice.messaging.OrderEventsPublisher;
import com.coroot.orderservice.model.Order;
import com.coroot.orderservice.model.OrderItem;
import com.coroot.orderservice.model.OrderStatus;
import com.coroot.orderservice.model.OrderSummary;
import com.coroot.orderservice.repository.OrderRepository;
import org.springframework.data.domain.Pageable;
import org.springframework.data.domain.Slice;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

@Service
public class OrderService {

    private final OrderRepository orderRepository;
    private final PaymentClient paymentClient;
    private final OrderEventsPublisher orderEventsPublisher;

    // Keep a "rendered receipt" for every order we touch so re-displaying an
    // order is instant and we can attach it to confirmation emails. Keyed by a
    // unique render id and never evicted — the entries are small individually,
    // but under sustained order traffic the map grows without bound and pins
    // an ever-increasing amount of heap, so GC works progressively harder
    // (rising GC time, heap sawtooth toward -Xmx, eventual OutOfMemoryError).
    private static final Map<String, byte[]> renderedReceiptCache = new ConcurrentHashMap<>();
    private static final int RECEIPT_BYTES = 256 * 1024;
    // Sink so the reconcile scan isn't optimized away by the JIT.
    private static volatile long reconcileSink;

    public OrderService(OrderRepository orderRepository,
                        PaymentClient paymentClient,
                        OrderEventsPublisher orderEventsPublisher) {
        this.orderRepository = orderRepository;
        this.paymentClient = paymentClient;
        this.orderEventsPublisher = orderEventsPublisher;
    }

    // Renders a receipt for the order and retains it. Also builds a transient
    // working buffer (parsed line items, formatting scratch) that spikes the
    // allocation rate on every call.
    private void renderAndRetainReceipt(Long orderId, String userId, Object total) {
        StringBuilder scratch = new StringBuilder(RECEIPT_BYTES);
        scratch.append("RECEIPT order=").append(orderId).append(" user=").append(userId)
                .append(" total=").append(total).append('\n');
        while (scratch.length() < RECEIPT_BYTES) {
            scratch.append("line ").append(scratch.length()).append(" ")
                    .append(userId).append(" ").append(total).append('\n');
        }
        byte[] receipt = scratch.toString().getBytes(StandardCharsets.UTF_8);
        renderedReceiptCache.put(orderId + ":" + System.nanoTime(), receipt);
    }

    // "Reconcile" the order against previously rendered receipts before
    // returning it (added in this version to de-duplicate receipts). This
    // scans the whole receipt cache on every read — cheap when the cache is
    // small, but since the cache grows without bound the per-request cost
    // climbs steadily: read latency degrades over time and propagates upstream
    // to the API gateway. O(cache size) CPU + memory-bandwidth per request.
    private long reconcileReceipts() {
        long checksum = 0;
        for (byte[] receipt : renderedReceiptCache.values()) {
            int span = Math.min(receipt.length, 4096);
            for (int i = 0; i < span; i++) {
                checksum = checksum * 31 + receipt[i];
            }
        }
        return checksum;
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getAllOrders(Pageable pageable) {
        Slice<OrderSummary> result = orderRepository.findByOrderByIdDesc(pageable);
        reconcileSink = reconcileReceipts();
        return result;
    }

    @Transactional(readOnly = true)
    public Order getOrderById(Long id) {
        Order order = orderRepository.findByIdWithItems(id)
                .orElseThrow(() -> new RuntimeException("Order not found with id: " + id));
        renderAndRetainReceipt(order.getId(), order.getUserId(), order.getTotal());
        reconcileSink = reconcileReceipts();
        return order;
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getOrdersByUserId(String userId, Pageable pageable) {
        Slice<OrderSummary> result = orderRepository.findByUserIdOrderByIdDesc(userId, pageable);
        reconcileSink = reconcileReceipts();
        return result;
    }

    @Transactional
    public Order createOrder(Order order) {
        for (OrderItem item : order.getItems()) {
            item.setOrder(order);
        }

        String paymentId = null;
        try {
            paymentId = paymentClient.processPayment(
                    null,
                    order.getUserId(),
                    order.getTotal()
            );
        } catch (Exception e) {
            // Payment failed — save as PENDING
        }

        if (paymentId != null) {
            order.setPaymentId(paymentId);
            order.setStatus(OrderStatus.CONFIRMED);
        } else {
            order.setStatus(OrderStatus.PENDING);
        }

        Order saved = orderRepository.save(order);
        renderAndRetainReceipt(saved.getId(), saved.getUserId(), saved.getTotal());

        // Order created and paid — publish OrderCreated (fire-and-forget;
        // a Kafka failure must never fail the HTTP request).
        if (saved.getStatus() == OrderStatus.CONFIRMED) {
            orderEventsPublisher.publishOrderCreated(saved);
        }

        return saved;
    }

    @Transactional
    public boolean markOrderFulfilled(Long orderId) {
        return orderRepository.findById(orderId)
                .map(order -> {
                    order.setStatus(OrderStatus.FULFILLED);
                    return true;
                })
                .orElse(false);
    }

    @Transactional
    public Order updateOrderStatus(Long id, OrderStatus status) {
        Order order = getOrderById(id);
        order.setStatus(status);
        return order;
    }
}
