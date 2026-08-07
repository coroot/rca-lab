FROM --platform=linux/amd64 rust:latest AS builder
WORKDIR /app
COPY Cargo.toml ./
RUN mkdir src && echo "fn main() {}" > src/main.rs && cargo build --release && rm -rf src
COPY src/ src/
COPY variants/ variants/
ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./src/; fi
RUN touch src/main.rs && cargo build --release

FROM --platform=linux/amd64 debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/target/release/payment-service /usr/local/bin/
EXPOSE 8080
CMD ["payment-service"]
