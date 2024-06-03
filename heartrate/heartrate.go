package heartrate

import (
    "encoding/hex"
    "errors"
    "os"
    "strings"
    "time"

    "github.com/sirupsen/logrus"
    "tinygo.org/x/bluetooth"
)

type HeartRatePayload struct {
    HeartRate int       `json:"heart_rate"`
    Timestamp time.Time `json:"timestamp"`
}

const (
    HeartRateServiceUUID       = "0000180d-0000-1000-8000-00805f9b34fb"
    HeartRateCharacteristicUUID = "00002a37-0000-1000-8000-00805f9b34fb"
)

var log = logrus.New()

func init() {
    log.Out = os.Stdout
    log.SetLevel(logrus.InfoLevel)
}

func ScanAndConnect(config Config) (*bluetooth.Device, error) {
    adapter := bluetooth.DefaultAdapter

    if err := adapter.Enable(); err != nil {
        return nil, wrapError(err, "enable BLE stack")
    }

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
        if err != nil {
            log.Errorf("start scan: %v", err)
        }
    }()

    var device bluetooth.ScanResult
    select {
    case device = <-ch:
        log.Infof("Connecting to device: %s (%s)", device.Address.String(), device.LocalName())
    case <-time.After(time.Duration(config.ScanTimeout) * time.Second):
        return nil, errors.New("timeout while scanning for devices")
    case <-stop:
    }

    if err := adapter.StopScan(); err != nil {
        return nil, wrapError(err, "stop scan")
    }

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
        return nil, errors.New("failed to connect to device after multiple attempts")
    }
    log.Info("Connected to the device")
    return peer, nil
}

func DiscoverServices(peer *bluetooth.Device) ([]bluetooth.DeviceService, error) {
    log.Info("Discovering services...")
    serviceUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateServiceUUID))
    services, err := peer.DiscoverServices([]bluetooth.UUID{serviceUUID})
    if err != nil {
        return nil, wrapError(err, "discover services")
    }
    if len(services) == 0 {
        return nil, errors.New("no services found")
    }
    log.Info("Services discovered")
    return services, nil
}

func DiscoverCharacteristics(service bluetooth.DeviceService) ([]bluetooth.DeviceCharacteristic, error) {
    log.Info("Discovering characteristics...")
    characteristicUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateCharacteristicUUID))
    characteristics, err := service.DiscoverCharacteristics([]bluetooth.UUID{characteristicUUID})
    if err != nil {
        return nil, wrapError(err, "discover characteristics")
    }
    if len(characteristics) == 0 {
        return nil, errors.New("no characteristics found")
    }
    log.Info("Characteristics discovered")
    return characteristics, nil
}

func EnableNotifications(characteristic bluetooth.DeviceCharacteristic) error {
    log.Info("Enabling notifications...")
    err := characteristic.EnableNotifications(handleNotification)
    if err != nil {
        return wrapError(err, "enable notifications")
    }
    log.Info("Notifications enabled")
    return nil
}

func SubscribeHeartRateData(characteristic bluetooth.DeviceCharacteristic, dataStream chan<- HeartRatePayload) error {
    log.Info("Subscribing to heart rate data...")
    err := characteristic.EnableNotifications(func(buf []byte) {
        if len(buf) > 0 {
            hr := buf[1]
            payload := HeartRatePayload{
                HeartRate: int(hr),
                Timestamp: time.Now().UTC(),
            }
            dataStream <- payload
        } else {
            log.Warn("Received empty notification buffer")
        }
    })
    if err != nil {
        return wrapError(err, "subscribe to heart rate data")
    }
    log.Info("Subscribed to heart rate data")
    return nil
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
    if config.TargetDeviceMAC != "" && result.Address.String() == config.TargetDeviceMAC {
        return true
    }
    if config.TargetDeviceName != "" && strings.Contains(result.LocalName(), config.TargetDeviceName) {
        return true
    }
    return false
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

func wrapError(err error, context string) error {
    return errors.New(context + ": " + err.Error())
}
