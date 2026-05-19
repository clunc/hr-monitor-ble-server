package main

import (
    "github.com/sirupsen/logrus"
    "github.com/clunc/hr-monitor-ble-server/pkg/heartrate"
)

func main() {
    config := heartrate.Config{
        TargetDeviceName: "Polar H10",
        ScanTimeout:      30,
    }

    hrm := heartrate.NewHeartRateMonitor(config)
    hrm.Start()

    for data := range hrm.Subscribe() {
        logrus.Infof("Heart rate: %d bpm", data.HeartRate)
    }

    hrm.Stop()
}
