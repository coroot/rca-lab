package com.coroot.orderservice.messaging;

import com.coroot.orderservice.service.OrderService;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;

/**
 * Consumes ShipmentCreated events from the {@code shipment-events} topic
 * (published by the fulfillment-service) and marks the corresponding
 * order as FULFILLED.
 */
@Component
public class ShipmentEventsListener {

    private static final Logger log = LoggerFactory.getLogger(ShipmentEventsListener.class);

    private final OrderService orderService;
    private final ObjectMapper objectMapper;

    public ShipmentEventsListener(OrderService orderService, ObjectMapper objectMapper) {
        this.orderService = orderService;
        this.objectMapper = objectMapper;
    }

    @KafkaListener(topics = "shipment-events", groupId = "order-service")
    public void onShipmentEvent(String payload) {
        try {
            JsonNode event = objectMapper.readTree(payload);
            String eventType = event.path("eventType").asText();
            if (!"ShipmentCreated".equals(eventType)) {
                log.debug("Ignoring shipment event of type '{}'", eventType);
                return;
            }
            long orderId = event.path("orderId").asLong();
            if (orderId <= 0) {
                log.warn("ShipmentCreated event without a valid orderId: {}", payload);
                return;
            }
            boolean updated = orderService.markOrderFulfilled(orderId);
            if (updated) {
                log.info("Order {} marked FULFILLED (shipmentId={})", orderId, event.path("shipmentId").asLong());
            } else {
                log.warn("Received ShipmentCreated for unknown order {} — ignoring", orderId);
            }
        } catch (Exception e) {
            log.error("Failed to process shipment event: {}", payload, e);
        }
    }
}
