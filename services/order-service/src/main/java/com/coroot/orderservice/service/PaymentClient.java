package com.coroot.orderservice.service;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.web.client.RestTemplateBuilder;
import org.springframework.http.HttpEntity;
import org.springframework.http.HttpHeaders;
import org.springframework.http.MediaType;
import org.springframework.stereotype.Component;
import org.springframework.web.client.RestTemplate;

import java.math.BigDecimal;
import java.time.Duration;
import java.util.Map;
import java.util.UUID;

@Component
public class PaymentClient {

    private final RestTemplate restTemplate;
    private final String paymentServiceUrl;

    public PaymentClient(
            RestTemplateBuilder builder,
            @Value("${payment.service.url:http://localhost:8081}") String paymentServiceUrl) {
        this.restTemplate = builder
                .setConnectTimeout(Duration.ofSeconds(5))
                .setReadTimeout(Duration.ofSeconds(10))
                .build();
        this.paymentServiceUrl = paymentServiceUrl;
    }

    @SuppressWarnings("unchecked")
    public String processPayment(Long orderId, String userId, BigDecimal amount) {
        HttpHeaders headers = new HttpHeaders();
        headers.setContentType(MediaType.APPLICATION_JSON);

        Map<String, Object> body = Map.of(
                "order_id", orderId != null ? "order-" + orderId : "order-" + UUID.randomUUID(),
                "user_id", userId,
                "amount", amount,
                "currency", "USD",
                "method", "card"
        );

        HttpEntity<Map<String, Object>> request = new HttpEntity<>(body, headers);

        Map<String, Object> response = restTemplate.postForObject(
                paymentServiceUrl + "/payments",
                request,
                Map.class
        );

        if (response != null && response.containsKey("id")) {
            return response.get("id").toString();
        }

        throw new RuntimeException("Payment processing failed");
    }
}
