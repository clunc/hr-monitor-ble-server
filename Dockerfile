# Stage 1: Build
FROM golang:1.26 AS builder
WORKDIR /app
RUN apt-get update && apt-get install -y libbluetooth-dev libdbus-1-dev && rm -rf /var/lib/apt/lists/*
COPY . .
RUN go mod download
RUN go build -o hr-monitor-ble-server .

# Stage 2: Runtime
FROM ubuntu:22.04
WORKDIR /app
COPY --from=builder /app/hr-monitor-ble-server /usr/local/bin/hr-monitor-ble-server
RUN apt-get update && apt-get install -y \
    bluez \
    dbus \
    && rm -rf /var/lib/apt/lists/*
RUN dbus-uuidgen > /var/lib/dbus/machine-id

CMD ["hr-monitor-ble-server"]
