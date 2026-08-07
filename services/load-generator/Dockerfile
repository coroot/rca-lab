FROM --platform=linux/amd64 golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o load-generator .

FROM --platform=linux/amd64 alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/load-generator /usr/local/bin/
CMD ["load-generator"]
