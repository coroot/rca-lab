package com.coroot.orderservice.model;

import java.math.BigDecimal;
import java.time.LocalDateTime;

public interface OrderSummary {
    Long getId();
    String getUserId();
    OrderStatus getStatus();
    BigDecimal getTotal();
    String getPaymentId();
    LocalDateTime getCreatedAt();
    LocalDateTime getUpdatedAt();
}
