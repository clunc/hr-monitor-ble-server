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
