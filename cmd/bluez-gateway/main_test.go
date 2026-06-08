package main

import (
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

func TestParseHeartRateWithRRIntervals(t *testing.T) {
	measurement, ok := parseHeartRate([]byte{0x10, 72, 0x00, 0x04})
	if !ok {
		t.Fatal("expected valid measurement")
	}
	if measurement.heartRate != 72 {
		t.Fatalf("heart rate = %d, want 72", measurement.heartRate)
	}
	if len(measurement.rrIntervals) != 1 || measurement.rrIntervals[0] != 1000 {
		t.Fatalf("RR intervals = %v, want [1000]", measurement.rrIntervals)
	}
}

func TestParseHeartRateRejectsZero(t *testing.T) {
	if _, ok := parseHeartRate([]byte{0x00, 0x00}); ok {
		t.Fatal("expected zero heart rate to be rejected")
	}
}

func TestAdapterFromObjects(t *testing.T) {
	objects := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci1": {
			"org.bluez.Adapter1": {},
		},
	}

	path, err := adapterFromObjects(objects)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/org/bluez/hci1" {
		t.Fatalf("adapter path = %q, want /org/bluez/hci1", path)
	}
}
