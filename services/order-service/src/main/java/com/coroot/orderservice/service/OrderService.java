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

@Service
public class OrderService {

    private final OrderRepository orderRepository;
    private final PaymentClient paymentClient;
    private final OrderEventsPublisher orderEventsPublisher;

    public OrderService(OrderRepository orderRepository,
                        PaymentClient paymentClient,
                        OrderEventsPublisher orderEventsPublisher) {
        this.orderRepository = orderRepository;
        this.paymentClient = paymentClient;
        this.orderEventsPublisher = orderEventsPublisher;
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getAllOrders(Pageable pageable) {
        return orderRepository.findByOrderByIdDesc(pageable);
    }

    @Transactional(readOnly = true)
    public Order getOrderById(Long id) {
        return orderRepository.findByIdWithItems(id)
                .orElseThrow(() -> new RuntimeException("Order not found with id: " + id));
    }

    @Transactional(readOnly = true)
    public Slice<OrderSummary> getOrdersByUserId(String userId, Pageable pageable) {
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
