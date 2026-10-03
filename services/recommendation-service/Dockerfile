FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

RUN CGO_ENABLED=0 GOOS=linux go build -o /recommendation-service .

FROM alpine:3.19
COPY --from=builder /recommendation-service /recommendation-service
EXPOSE 9000
ENTRYPOINT ["/recommendation-service"]
