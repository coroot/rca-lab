# Build the rca-lab operator binary (also contains the dbtool subcommand
# used by Workload scenario Jobs).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rca-lab ./cmd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rca-lab /rca-lab
USER nonroot:nonroot
ENTRYPOINT ["/rca-lab"]
CMD ["manager"]
