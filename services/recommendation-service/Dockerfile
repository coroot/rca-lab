FROM --platform=linux/amd64 golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /recommendation-service .

FROM --platform=linux/amd64 alpine:3.19
COPY --from=builder /recommendation-service /recommendation-service
EXPOSE 9000
ENTRYPOINT ["/recommendation-service"]
