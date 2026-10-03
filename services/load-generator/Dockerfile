FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

RUN CGO_ENABLED=0 GOOS=linux go build -o load-generator .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/load-generator /usr/local/bin/
CMD ["load-generator"]
