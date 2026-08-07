FROM --platform=linux/amd64 golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o fulfillment-service .

FROM --platform=linux/amd64 alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/fulfillment-service /usr/local/bin/
EXPOSE 8080
CMD ["fulfillment-service"]
