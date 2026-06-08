package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	dbus "github.com/godbus/dbus/v5"
	"github.com/sirupsen/logrus"
)

const (
	heartRateServiceUUID        = "0000180d-0000-1000-8000-00805f9b34fb"
	heartRateCharacteristicUUID = "00002a37-0000-1000-8000-00805f9b34fb"
)

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	targetMAC := getenvDefault("TARGET_MAC", "C9:9C:80:01:00:92")
	targetName := getenvDefault("TARGET_NAME", "Polar H10")
	scanTimeout := time.Duration(getenvIntDefault("SCAN_TIMEOUT_SECONDS", 30)) * time.Second

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	for {
		if err := collect(targetMAC, targetName, scanTimeout, stop); err != nil {
			logrus.Errorf("BLE session failed: %v", err)
		}

		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func collect(targetMAC, targetName string, scanTimeout time.Duration, stop <-chan os.Signal) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("connect system D-Bus: %w", err)
	}
	defer conn.Close()

	adapterPath, err := findAdapter(conn)
	if err != nil {
		return err
	}
	adapter := conn.Object("org.bluez", adapterPath)
	if call := adapter.Call("org.bluez.Adapter1.StartDiscovery", 0); call.Err != nil && !strings.Contains(call.Err.Error(), "InProgress") {
		return fmt.Errorf("start discovery: %w", call.Err)
	}
	defer adapter.Call("org.bluez.Adapter1.StopDiscovery", 0)

	logrus.Infof("Scanning for %s (%s)...", targetName, targetMAC)
	devicePath, err := waitForDevice(conn, targetMAC, scanTimeout, stop)
	if err != nil {
		return err
	}

	device := conn.Object("org.bluez", devicePath)
	if call := device.Call("org.bluez.Device1.Connect", 0); call.Err != nil && !strings.Contains(call.Err.Error(), "AlreadyConnected") {
		return fmt.Errorf("connect device: %w", call.Err)
	}
	defer device.Call("org.bluez.Device1.Disconnect", 0)

	if err := waitForServicesResolved(conn, devicePath, scanTimeout, stop); err != nil {
		return err
	}

	heartRatePath, err := findHeartRateCharacteristic(conn, devicePath)
	if err != nil {
		return err
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(heartRatePath),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		return fmt.Errorf("subscribe to D-Bus signals: %w", err)
	}
	defer conn.RemoveMatchSignal(
		dbus.WithMatchObjectPath(heartRatePath),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	)

	characteristic := conn.Object("org.bluez", heartRatePath)
	if call := characteristic.Call("org.bluez.GattCharacteristic1.StartNotify", 0); call.Err != nil && !strings.Contains(call.Err.Error(), "Already notifying") {
		return fmt.Errorf("start notifications: %w", call.Err)
	}
	defer characteristic.Call("org.bluez.GattCharacteristic1.StopNotify", 0)

	logrus.Infof("Streaming heart rate from %s", targetName)
	signals := make(chan *dbus.Signal, 32)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	for {
		select {
		case <-stop:
			return nil
		case signal := <-signals:
			measurement, ok := measurementFromSignal(signal, heartRatePath)
			if !ok {
				continue
			}
			if len(measurement.rrIntervals) > 0 {
				logrus.Infof("Heart rate: %d bpm | RR: %v ms", measurement.heartRate, measurement.rrIntervals)
			} else {
				logrus.Infof("Heart rate: %d bpm", measurement.heartRate)
			}
		case <-time.After(10 * time.Second):
			return fmt.Errorf("no heart-rate notification for 10 seconds")
		}
	}
}

type measurement struct {
	heartRate   int
	rrIntervals []uint32
}

func measurementFromSignal(signal *dbus.Signal, path dbus.ObjectPath) (measurement, bool) {
	if signal == nil || signal.Path != path || signal.Name != "org.freedesktop.DBus.Properties.PropertiesChanged" || len(signal.Body) < 2 {
		return measurement{}, false
	}

	changed, ok := signal.Body[1].(map[string]dbus.Variant)
	if !ok {
		return measurement{}, false
	}
	raw, ok := changed["Value"].Value().([]byte)
	if !ok {
		return measurement{}, false
	}
	return parseHeartRate(raw)
}

func parseHeartRate(raw []byte) (measurement, bool) {
	if len(raw) < 2 {
		return measurement{}, false
	}

	flags := raw[0]
	offset := 1
	result := measurement{}
	if flags&0x01 == 0 {
		result.heartRate = int(raw[offset])
		offset++
	} else {
		if len(raw) < offset+2 {
			return measurement{}, false
		}
		result.heartRate = int(binary.LittleEndian.Uint16(raw[offset : offset+2]))
		offset += 2
	}
	if result.heartRate == 0 {
		return measurement{}, false
	}

	if flags&0x08 != 0 {
		if len(raw) < offset+2 {
			return measurement{}, false
		}
		offset += 2
	}
	if flags&0x10 != 0 {
		for offset+1 < len(raw) {
			rr := binary.LittleEndian.Uint16(raw[offset : offset+2])
			result.rrIntervals = append(result.rrIntervals, uint32(rr)*1000/1024)
			offset += 2
		}
	}
	return result, true
}

func waitForDevice(conn *dbus.Conn, targetMAC string, timeout time.Duration, stop <-chan os.Signal) (dbus.ObjectPath, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		path, found, err := findDevice(conn, targetMAC)
		if err != nil {
			return "", err
		}
		if found {
			return path, nil
		}
		select {
		case <-stop:
			return "", fmt.Errorf("stopped")
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("device %s not found", targetMAC)
}

func findDevice(conn *dbus.Conn, targetMAC string) (dbus.ObjectPath, bool, error) {
	objects, err := managedObjects(conn)
	if err != nil {
		return "", false, err
	}
	for path, interfaces := range objects {
		properties, ok := interfaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		address, ok := properties["Address"].Value().(string)
		if ok && strings.EqualFold(address, targetMAC) {
			return path, true, nil
		}
	}
	return "", false, nil
}

func findAdapter(conn *dbus.Conn) (dbus.ObjectPath, error) {
	objects, err := managedObjects(conn)
	if err != nil {
		return "", err
	}
	return adapterFromObjects(objects)
}

func adapterFromObjects(objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant) (dbus.ObjectPath, error) {
	for path, interfaces := range objects {
		if _, ok := interfaces["org.bluez.Adapter1"]; ok {
			return path, nil
		}
	}
	return "", fmt.Errorf("Bluetooth adapter not found")
}

func waitForServicesResolved(conn *dbus.Conn, path dbus.ObjectPath, timeout time.Duration, stop <-chan os.Signal) error {
	deadline := time.Now().Add(timeout)
	device := conn.Object("org.bluez", path)
	for time.Now().Before(deadline) {
		var properties map[string]dbus.Variant
		if err := device.Call("org.freedesktop.DBus.Properties.GetAll", 0, "org.bluez.Device1").Store(&properties); err == nil {
			if resolved, ok := properties["ServicesResolved"].Value().(bool); ok && resolved {
				return nil
			}
		}
		select {
		case <-stop:
			return fmt.Errorf("stopped")
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("services not resolved for %s", path)
}

func findHeartRateCharacteristic(conn *dbus.Conn, devicePath dbus.ObjectPath) (dbus.ObjectPath, error) {
	objects, err := managedObjects(conn)
	if err != nil {
		return "", err
	}
	for path, interfaces := range objects {
		characteristic, ok := interfaces["org.bluez.GattCharacteristic1"]
		if !ok || !strings.HasPrefix(string(path), string(devicePath)) {
			continue
		}
		uuid, _ := characteristic["UUID"].Value().(string)
		service, _ := characteristic["Service"].Value().(dbus.ObjectPath)
		serviceProperties := objects[service]["org.bluez.GattService1"]
		serviceUUID, _ := serviceProperties["UUID"].Value().(string)
		if strings.EqualFold(uuid, heartRateCharacteristicUUID) && strings.EqualFold(serviceUUID, heartRateServiceUUID) {
			return path, nil
		}
	}
	return "", fmt.Errorf("heart-rate characteristic not found")
}

func managedObjects(conn *dbus.Conn) (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := conn.Object("org.bluez", "/").Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects); err != nil {
		return nil, fmt.Errorf("get BlueZ objects: %w", err)
	}
	return objects, nil
}

func getenvDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func getenvIntDefault(name string, fallback int) int {
	value := os.Getenv(name)
	parsed, err := strconv.Atoi(value)
	if value == "" || err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
