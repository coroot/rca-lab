package com.coroot.orderservice.messaging;

import com.coroot.orderservice.model.Order;
import com.coroot.orderservice.model.OrderItem;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.kafka.core.KafkaTemplate;
import org.springframework.stereotype.Component;

import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;

/**
 * Publishes OrderCreated events to the {@code order-events} topic.
 *
 * Fire-and-forget: any failure to build or send the event is logged and
 * swallowed so that Kafka problems never fail the HTTP request.
 */
@Component
public class OrderEventsPublisher {

    private static final Logger log = LoggerFactory.getLogger(OrderEventsPublisher.class);

    private static final String TOPIC = "order-events";

    private final KafkaTemplate<String, String> kafkaTemplate;
    private final ObjectMapper objectMapper;

    public OrderEventsPublisher(KafkaTemplate<String, String> kafkaTemplate, ObjectMapper objectMapper) {
        this.kafkaTemplate = kafkaTemplate;
        this.objectMapper = objectMapper;
    }

    public void publishOrderCreated(Order order) {
        Long orderId = order.getId();
        try {
            ObjectNode event = objectMapper.createObjectNode();
            event.put("eventType", "OrderCreated");
            event.put("orderId", orderId);
            event.put("userId", order.getUserId());
            ArrayNode items = event.putArray("items");
            for (OrderItem item : order.getItems()) {
                ObjectNode itemNode = items.addObject();
                itemNode.put("productId", item.getProductId());
                itemNode.put("quantity", item.getQuantity());
                itemNode.put("price", item.getPrice());
            }
            event.put("totalAmount", order.getTotal());
            event.put("createdAt", order.getCreatedAt()
                    .atOffset(ZoneOffset.UTC)
                    .format(DateTimeFormatter.ISO_OFFSET_DATE_TIME));

            String payload = objectMapper.writeValueAsString(event);

            kafkaTemplate.send(TOPIC, String.valueOf(orderId), payload)
                    .whenComplete((result, ex) -> {
                        if (ex != null) {
                            log.error("Failed to publish OrderCreated event for order {}", orderId, ex);
                        } else {
                            log.debug("Published OrderCreated event for order {} to {}-{}",
                                    orderId,
                                    result.getRecordMetadata().topic(),
                                    result.getRecordMetadata().partition());
                        }
                    });
        } catch (Exception e) {
            log.error("Failed to publish OrderCreated event for order {}", orderId, e);
        }
    }
}
