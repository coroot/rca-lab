package com.coroot.orderservice.repository;

import com.coroot.orderservice.model.Order;
import com.coroot.orderservice.model.OrderSummary;
import org.springframework.data.domain.Pageable;
import org.springframework.data.domain.Slice;
import org.springframework.data.jpa.repository.JpaRepository;
import org.springframework.data.jpa.repository.Query;
import org.springframework.stereotype.Repository;

import java.util.Optional;

@Repository
public interface OrderRepository extends JpaRepository<Order, Long> {

    @Query("SELECT o FROM Order o LEFT JOIN FETCH o.items WHERE o.id = :id")
    Optional<Order> findByIdWithItems(Long id);

    Slice<OrderSummary> findByOrderByIdDesc(Pageable pageable);

    Slice<OrderSummary> findByUserIdOrderByIdDesc(String userId, Pageable pageable);
}
