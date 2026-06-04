package main

import (
	"github.com/clunc/hr-monitor-ble-server/pkg/heartrate"
	"github.com/sirupsen/logrus"
)

func main() {
	config := heartrate.Config{
		TargetDeviceName: "Polar H10",
		ScanTimeout:      30,
	}

	hrm := heartrate.NewHeartRateMonitor(config)
	hrm.Start()

	for data := range hrm.Subscribe() {
		if len(data.GetRrIntervals()) > 0 {
			logrus.Infof("Heart rate: %d bpm | RR: %v ms", data.GetHeartRate(), data.GetRrIntervals())
		} else {
			logrus.Infof("Heart rate: %d bpm", data.GetHeartRate())
		}
	}

	hrm.Stop()
}
