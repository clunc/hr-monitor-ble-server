package main

import (
    "log"

    "hr-monitor-ble-server/heartrate"
)

func main() {
    config, err := heartrate.LoadConfig("config.json")
    if err != nil {
        log.Fatalf("Failed to load config: %v", err)
    }

    peer, err := heartrate.ScanAndConnect(config)
    if err != nil {
        log.Fatalf("Failed to scan and connect: %v", err)
    }
    defer peer.Disconnect()

    services, err := heartrate.DiscoverServices(peer)
    if err != nil {
        log.Fatalf("Failed to discover services: %v", err)
    }

    characteristics, err := heartrate.DiscoverCharacteristics(services[0])
    if err != nil {
        log.Fatalf("Failed to discover characteristics: %v", err)
    }

    dataStream := make(chan heartrate.HeartRatePayload)

    err = heartrate.SubscribeHeartRateData(characteristics[0], dataStream)
    if err != nil {
        log.Fatalf("Failed to subscribe to heart rate data: %v", err)
    }

    go func() {
        for data := range dataStream {
            log.Printf("Received heart rate data: %d bpm at %s", data.HeartRate, data.Timestamp)
        }
    }()

    // Keep the program running.
    select {}
}
