package main

import (
	"os"
	"strconv"

	"github.com/clunc/hr-monitor-ble-server/pkg/heartrate"
	"github.com/sirupsen/logrus"
)

func main() {
	targetName := getenvDefault("TARGET_NAME", "Polar H10")
	targetMAC := os.Getenv("TARGET_MAC")
	scanTimeout := getenvIntDefault("SCAN_TIMEOUT_SECONDS", 30)

	config := heartrate.Config{
		TargetDeviceName: targetName,
		TargetDeviceMAC:  targetMAC,
		ScanTimeout:      scanTimeout,
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

func getenvDefault(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func getenvIntDefault(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		logrus.Warnf("Invalid %s value %q, using default %d", name, value, fallback)
		return fallback
	}

	return parsed
}
