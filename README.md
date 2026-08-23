# hr-monitor-ble-server

A Go project to retrieve data from Bluetooth Low Energy (BLE) heart rate monitors and make it available to message brokers or databases.

![Project Logo](path/to/logo.png)

## Table of Contents
- [Overview](#overview)
- [Features](#features)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
  - [Docker](#docker)
  - [Local Setup](#local-setup)
- [Configuration](#configuration)
- [Usage](#usage)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

## Overview
`hr-monitor-ble-server` is a server application designed to connect to BLE heart rate monitors, collect heart rate data, and make it available for further processing or integration with other systems. The application uses Go and is containerized using Docker for easy deployment.

## Features
- Connects to specified BLE heart rate monitors.
- Retrieves heart rate data and logs it.
- Easy configuration via JSON file.
- Dockerized for consistent deployment across environments.

## Prerequisites
- Docker and Docker Compose
- Go 1.22 or later
- A BLE heart rate monitor (e.g., Polar H10)

## Installation

### Docker
1. Clone the repository:
    ```sh
    git clone https://github.com/yourusername/hr-monitor-ble-server.git
    cd hr-monitor-ble-server
    ```

2. Build and run the Docker container:
    ```sh
    docker-compose up --build
    ```

### Local Setup (Without Docker)
1. Clone the repository:
    ```sh
    git clone https://github.com/yourusername/hr-monitor-ble-server.git
    cd hr-monitor-ble-server
    ```

2. Install dependencies:
    ```sh
    go mod tidy
    ```

3. Build and run the application:
    ```sh
    go build -o hr-monitor-ble-server
    ./hr-monitor-ble-server
    ```

## Configuration
Configuration is managed through the `config.json` file. Here is an example configuration:

```json
{
    "TargetDeviceName": "Polar H10",
    "TargetDeviceMAC": "XX:XX:XX:XX:XX:XX",
    "ScanTimeout": 60
}

## Control API

The gateway boots **idle**: it holds no BLE discovery session and touches no radio
until asked. Holding a strap around the clock drains it and keeps it out of reach
of a phone or watch, so the link is acquired on demand.

| endpoint | purpose |
|---|---|
| `GET /` | control page (device picker, connect/disconnect, live bpm) |
| `GET /status` | link state, target, bpm, battery, bond state, last error |
| `GET /devices` | scan hits; heart-rate straps (0x180D) first |
| `POST /connect` | acquire a link; optional `?name=` / `?mac=` to pick a device |
| `POST /disconnect` | drop the link, abort an in-flight scan |

`link` is one of `idle` → `scanning` → `connecting` → `linking` → `waiting` →
`connected`. `waiting` means subscribed but nothing streaming yet — see below.

### Environment

| var | default | meaning |
|---|---|---|
| `HTTP_ADDR` | `:8080` | control API / page |
| `KAFKA_BROKER`, `TOPIC` | unset | optional; log-only without them |
| `TARGET_NAME` | `Polar H10` | substring match on the advertised name |
| `TARGET_MAC` | unset | exact address; wearables rotate these, prefer the name |
| `AUTO_CONNECT` | `false` | acquire a link at boot instead of on demand |
| `HR_SOURCE` | slug of `TARGET_NAME` | label stamped on every measurement |
| `BLE_ADAPTER` | auto | preferred adapter; falls back to the first BlueZ reports |
| `HR_DATA_TIMEOUT_SECONDS` | 20 | reconnect after a stream that *was* flowing stops |
| `HR_CONNECT_DEADLINE_SECONDS` | 300 | give up hunting and release the radio |
| `HR_PAIR_TIMEOUT_SECONDS` | 28 | must stay under the SMP spec timeout (30s) |
| `HR_SUBSCRIBE_TIMEOUT_SECONDS` | 25 | bounds a GATT subscribe that would otherwise hang |

## Strap behaviour

**Polar H10** — streams unbonded. Connect and subscribe, nothing else; battery
level is exposed. Six seconds from cold start to beats.

**Fitbit Charge 6** — needs a **bond** before it will send anything (permitted by
the Heart Rate Profile spec, but rare), and Google encrypts the stream, which is
why it is advertised as working only with Peloton/NordicTrack/Tonal. It works
here because the gateway registers its own BlueZ pairing agent and bonds before
subscribing. Three caveats:

- The stream only flows while **HR on Equipment** is active on the watch (swipe
  down, tap it). Starting a workout is *not* enough. The gateway holds a silent
  subscription open in the `waiting` state for exactly this.
- **One receiver at a time** — if a phone holds the broadcast, the gateway can't.
- The bond lives per adapter, so the first connect from a new host needs someone
  to accept the prompt on the watch.

Headless hosts have no desktop pairing agent, which is why the gateway ships its
own (`pkg/heartrate/pairing.go`, `NoInputNoOutput` / Just Works).

## Diagnostics

`cmd/gattprobe` connects and subscribes to `0x2A37` straight over BlueZ D-Bus —
no tinygo, no bluetoothctl — and prints the characteristic flags plus raw beats.
Use it when the gateway misbehaves to find out whether the fault is the client or
the peripheral:

```bash
MAC=AA:BB:CC:DD:EE:FF LISTEN_S=60 go run ./cmd/gattprobe
```

Note BlueZ caches a device's GATT database, including for unpaired devices; a
stale empty cache looks exactly like a device that exposes no services.
`bluetoothctl remove <mac>` clears it.

## Running two straps at once

Each instance owns one strap, so a second one is a second process with its own
`HTTP_ADDR`, `TARGET_NAME` and `HR_SOURCE`. Both can publish to the same topic:
every measurement carries `source`, and rowing-stream-processor namespaces the
readings per strap (`hr_polar_h10_bpm`, `rr_charge_6`, ...).

One caveat — `rr_intervals`, the HRV series the processor actually consumes, can
only have one owner. Set `HR_PRIMARY_SOURCE` on the processor to name it; a strap
that reports no RR intervals never writes the key regardless, so a Fitbit
alongside a Polar needs no configuration at all.

A concurrent scan does not disturb an established link — verified with one
instance streaming while another scanned for 70s.
