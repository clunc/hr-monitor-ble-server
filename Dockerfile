# Stage 1: Build
FROM golang:1.26 AS builder
WORKDIR /app
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 go build -o hr-monitor-ble-server .

# Stage 2: Runtime
FROM scratch
COPY --from=builder /app/hr-monitor-ble-server /hr-monitor-ble-server
CMD ["/hr-monitor-ble-server"]
