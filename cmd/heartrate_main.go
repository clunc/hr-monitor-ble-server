package main

import (
    "log"
    "hr-monitor-ble-server/pkg/heartrate"
)

func main() {
    config := heartrate.Config{
        TargetDeviceName: "Polar H10",
        ScanTimeout:      30,
    }

    hrm := heartrate.NewHeartRateMonitor(config)
    hrm.Start()

    for data := range hrm.Subscribe() {
        log.Printf("Heart rate: %d bpm at %s", data.HeartRate, data.Timestamp)
    }

    hrm.Stop()
}
