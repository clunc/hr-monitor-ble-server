package main

import (
    "encoding/hex"
    "encoding/json"
    "errors"
    "io/ioutil"
    "os"
    "strings"
    "time"

    "github.com/sirupsen/logrus"
    "tinygo.org/x/bluetooth"
)

type Config struct {
    TargetDeviceName string `json:"TargetDeviceName"`
    TargetDeviceMAC  string `json:"TargetDeviceMAC"`
    ScanTimeout      int    `json:"ScanTimeout"`
}

type HeartRatePayload struct {
    HeartRate int       `json:"heart_rate"`
    Timestamp time.Time `json:"timestamp"`
}

const (
    HeartRateServiceUUID       = "0000180d-0000-1000-8000-00805f9b34fb" // Heart Rate Service UUID
    HeartRateCharacteristicUUID = "00002a37-0000-1000-8000-00805f9b34fb" // Heart Rate Measurement Characteristic UUID
)

var log = logrus.New()

func loadConfig() (Config, error) {
    var config Config
    file, err := os.Open("config.json")
    if err != nil {
        return config, wrapError(err, "opening config file")
    }
    defer file.Close()
    bytes, err := ioutil.ReadAll(file)
    if err != nil {
        return config, wrapError(err, "reading config file")
    }
    err = json.Unmarshal(bytes, &config)
    if err != nil {
        return config, wrapError(err, "unmarshalling config JSON")
    }
    return config, nil
}

func main() {
    log.Out = os.Stdout
    log.SetLevel(logrus.InfoLevel)

    config, err := loadConfig()
    if err != nil {
        log.Fatalf("Failed to load config: %v", err)
    }

    adapter := bluetooth.DefaultAdapter

    // Enable the Bluetooth adapter.
    must("enable BLE stack", adapter.Enable())

    // Scan for devices with a timeout.
    ch := make(chan bluetooth.ScanResult, 1)
    stop := make(chan struct{})
    go func() {
        err := adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
            log.Infof("Found device: %s (%s)", result.Address.String(), result.LocalName())
            if matchesTargetDevice(result, config) {
                ch <- result
                close(stop)
            }
        })
        must("start scan", err)
    }()

    var device bluetooth.ScanResult
    select {
    case device = <-ch:
        log.Infof("Connecting to device: %s (%s)", device.Address.String(), device.LocalName())
    case <-time.After(time.Duration(config.ScanTimeout) * time.Second):
        log.Warn("Timeout while scanning for devices")
        return
    case <-stop:
        // Scan stopped because the device was found
    }

    // Stop scanning.
    must("stop scan", adapter.StopScan())

    // Connect to the device with retries.
    log.Info("Attempting to connect to the device...")
    var peer *bluetooth.Device
    retryCount := 3
    for i := 0; i < retryCount; i++ {
        p, err := adapter.Connect(device.Address, bluetooth.ConnectionParams{})
        if err == nil {
            peer = &p
            break
        }
        log.Errorf("Failed to connect to device (attempt %d/%d): %v", i+1, retryCount, err)
        time.Sleep(2 * time.Second)
    }
    if peer == nil {
        log.Errorf("Failed to connect to device after %d attempts", retryCount)
        return
    }
    log.Info("Connected to the device")
    defer peer.Disconnect()

    // Discover services.
    log.Info("Discovering services...")
    serviceUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateServiceUUID))
    services, err := peer.DiscoverServices([]bluetooth.UUID{serviceUUID})
    if err != nil {
        log.Errorf("Failed to discover services: %v", err)
        return
    }
    if len(services) == 0 {
        log.Warn("No services found")
        return
    }
    log.Info("Services discovered")

    // Discover characteristics.
    log.Info("Discovering characteristics...")
    characteristicUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateCharacteristicUUID))
    characteristics, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{characteristicUUID})
    if err != nil {
        log.Errorf("Failed to discover characteristics: %v", err)
        return
    }
    if len(characteristics) == 0 {
        log.Warn("No characteristics found")
        return
    }
    log.Info("Characteristics discovered")

    // Subscribe to the heart rate measurement characteristic.
    log.Info("Enabling notifications...")
    err = characteristics[0].EnableNotifications(handleNotification)
    if err != nil {
        log.Errorf("Failed to enable notifications: %v", err)
        return
    }
    log.Info("Notifications enabled")

    // Keep the program running.
    select {}
}

func must(action string, err error) {
    if err != nil {
        log.Fatalf("Failed to %s: %v", action, err)
    }
}

func wrapError(err error, context string) error {
    return errors.New(context + ": " + err.Error())
}

func uuidToByteArray(uuid string) [16]byte {
    var ba [16]byte
    b, err := hex.DecodeString(uuid[0:8] + uuid[9:13] + uuid[14:18] + uuid[19:23] + uuid[24:])
    if err != nil {
        log.Errorf("Invalid UUID format: %v", err)
        return ba
    }
    copy(ba[:], b)
    return ba
}

func handleNotification(buf []byte) {
    if len(buf) > 0 {
        hr := buf[1]
        payload := HeartRatePayload{
            HeartRate: int(hr),
            Timestamp: time.Now().UTC(),
        }
        log.Infof("Heart rate: %d bpm at %s", payload.HeartRate, payload.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"))
    } else {
        log.Warn("Received empty notification buffer")
    }
}

func matchesTargetDevice(result bluetooth.ScanResult, config Config) bool {
    // Check if the device matches the target name (partial match) or exact MAC address
    if config.TargetDeviceMAC != "" && result.Address.String() == config.TargetDeviceMAC {
        return true
    }
    if config.TargetDeviceName != "" && strings.Contains(result.LocalName(), config.TargetDeviceName) {
        return true
    }
    return false
}
