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

import java.util.Queue;
import java.util.concurrent.ConcurrentLinkedQueue;

@Service
public class OrderService {

    private final OrderRepository orderRepository;
    private final PaymentClient paymentClient;
    private final OrderEventsPublisher orderEventsPublisher;

    // v1.4.0: in-memory "access audit trail" for the new compliance report.
    // Every read records a batch of fine-grained access entries into this
    // process-wide registry — but nothing ever prunes or flushes it. It is a
    // slow leak of many *small* objects rather than a few large ones: the
    // registry accumulates millions of tiny live entries, so the dominant cost
    // is the JVM having to trace an ever-growing live set on every GC. Old-gen
    // occupancy creeps up, G1 mark/mixed-collection time and frequency rise,
    // and order-service latency degrades gradually — long before the heap is
    // ever exhausted. A genuine leak introduced by a new version that manifests
    // first as a performance regression, not a crash.
    private static final Queue<long[]> ACCESS_AUDIT_TRAIL = new ConcurrentLinkedQueue<>();
    private static final int AUDIT_ENTRIES_PER_READ = 1500;

    public OrderService(OrderRepository orderRepository,
                        PaymentClient paymentClient,
                        OrderEventsPublisher orderEventsPublisher) {
        this.orderRepository = orderRepository;
        this.paymentClient = paymentClient;
        this.orderEventsPublisher = orderEventsPublisher;
    }

    // Records fine-grained access-audit entries for the compliance report.
    // Each entry is a small object; the batch is retained forever (leak).
    private void recordAccessAudit(long op, long id) {
        long ts = System.nanoTime();
        for (int i = 0; i < AUDIT_ENTRIES_PER_READ; i++) {
            ACCESS_AUDIT_TRAIL.add(new long[]{op, id, ts + i});
        }
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getAllOrders(Pageable pageable) {
        recordAccessAudit(1L, pageable.getPageNumber());
        return orderRepository.findByOrderByIdDesc(pageable);
    }

    @Transactional(readOnly = true)
    public Order getOrderById(Long id) {
        recordAccessAudit(2L, id);
        return orderRepository.findByIdWithItems(id)
                .orElseThrow(() -> new RuntimeException("Order not found with id: " + id));
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getOrdersByUserId(String userId, Pageable pageable) {
        recordAccessAudit(3L, userId.hashCode());
        return orderRepository.findByUserIdOrderByIdDesc(userId, pageable);
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
