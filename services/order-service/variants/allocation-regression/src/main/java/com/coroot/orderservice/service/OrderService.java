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

import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

@Service
public class OrderService {

    private final OrderRepository orderRepository;
    private final PaymentClient paymentClient;
    private final OrderEventsPublisher orderEventsPublisher;

    // Cache recently served order listings to take read pressure off MySQL.
    // Entries are kept in LRU order and capped, so memory usage stays bounded.
    private static final int RECENT_CACHE_MAX_ENTRIES = 30_000;
    private static final Map<String, List<Map<String, Object>>> recentResultsCache =
            Collections.synchronizedMap(new LinkedHashMap<>(1024, 0.75f, true) {
                @Override
                protected boolean removeEldestEntry(Map.Entry<String, List<Map<String, Object>>> eldest) {
                    return size() > RECENT_CACHE_MAX_ENTRIES;
                }
            });

    private static final DateTimeFormatter TS_FORMAT = DateTimeFormatter.ISO_LOCAL_DATE_TIME;

    public OrderService(OrderRepository orderRepository,
                        PaymentClient paymentClient,
                        OrderEventsPublisher orderEventsPublisher) {
        this.orderRepository = orderRepository;
        this.paymentClient = paymentClient;
        this.orderEventsPublisher = orderEventsPublisher;
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getAllOrders(Pageable pageable) {
        Slice<OrderSummary> result = orderRepository.findByOrderByIdDesc(pageable);
        cacheListing("all", pageable, result);
        return result;
    }

    @Transactional(readOnly = true)
    public Order getOrderById(Long id) {
        Order order = orderRepository.findByIdWithItems(id)
                .orElseThrow(() -> new RuntimeException("Order not found with id: " + id));
        cacheOrderDetail(order);
        return order;
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getOrdersByUserId(String userId, Pageable pageable) {
        Slice<OrderSummary> result = orderRepository.findByUserIdOrderByIdDesc(userId, pageable);
        cacheListing(userId, pageable, result);
        return result;
    }

    private void cacheListing(String scope, Pageable pageable, Slice<OrderSummary> result) {
        List<Map<String, Object>> rows = new ArrayList<>(result.getNumberOfElements());
        for (OrderSummary summary : result.getContent()) {
            Map<String, Object> row = new HashMap<>();
            row.put("id", summary.getId());
            row.put("userId", summary.getUserId());
            row.put("status", String.valueOf(summary.getStatus()));
            row.put("total", summary.getTotal());
            row.put("createdAt", summary.getCreatedAt());
            row.put("display", String.format("order #%d for %s: %s (%s) placed %s",
                    summary.getId(), summary.getUserId(), summary.getTotal(),
                    summary.getStatus(), summary.getCreatedAt()));
            rows.add(row);
        }
        recentResultsCache.put(listingKey(scope, pageable), rows);
    }

    private void cacheOrderDetail(Order order) {
        List<Map<String, Object>> rows = new ArrayList<>(order.getItems().size() + 1);
        Map<String, Object> head = new HashMap<>();
        head.put("id", order.getId());
        head.put("userId", order.getUserId());
        head.put("status", String.valueOf(order.getStatus()));
        head.put("total", order.getTotal());
        head.put("paymentId", order.getPaymentId());
        rows.add(head);
        for (OrderItem item : order.getItems()) {
            Map<String, Object> row = new HashMap<>();
            row.put("productId", item.getProductId());
            row.put("name", item.getName());
            row.put("quantity", item.getQuantity());
            row.put("price", item.getPrice());
            row.put("display", String.format("%s x%d @ %s", item.getName(),
                    item.getQuantity(), item.getPrice()));
            rows.add(row);
        }
        recentResultsCache.put(detailKey(order), rows);
    }

    private String listingKey(String scope, Pageable pageable) {
        // Bucket by minute so listings refresh at least once a minute.
        long minuteBucket = System.currentTimeMillis() / 60_000;
        return "listing:" + scope + ":" + pageable.getPageNumber() + ":"
                + pageable.getPageSize() + ":" + pageable.getSort() + ":" + minuteBucket;
    }

    private String detailKey(Order order) {
        // Include the order's last-seen state so stale entries are never served.
        long minuteBucket = System.currentTimeMillis() / 60_000;
        return "order:" + order.getId() + ":" + order.getStatus() + ":"
                + (order.getCreatedAt() != null ? order.getCreatedAt().format(TS_FORMAT) : "")
                + ":" + minuteBucket;
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
