# Stage 1: Build stage
FROM golang:1.22 as builder
WORKDIR /app
COPY . .
RUN go get tinygo.org/x/bluetooth
RUN go build -o hr-monitor-ble-server

# Stage 2: Runtime stage
FROM ubuntu:22.04
WORKDIR /app
COPY --from=builder /app/hr-monitor-ble-server /usr/local/bin/hr-monitor-ble-server
COPY config.json /app/config.json
RUN apt-get update && apt-get install -y \
    bluez \
    dbus \
    && rm -rf /var/lib/apt/lists/*
RUN dbus-uuidgen > /var/lib/dbus/machine-id

CMD ["hr-monitor-ble-server"]
