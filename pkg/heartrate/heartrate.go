package heartrate

import (
    "encoding/hex"
    "errors"
    "strings"
    "sync"
    "time"

    "github.com/sirupsen/logrus"
    "tinygo.org/x/bluetooth"
)

// HeartRatePayload represents the heart rate data along with the timestamp.
type HeartRatePayload struct {
    HeartRate int       `json:"heart_rate"`
    Timestamp time.Time `json:"timestamp"`
}

// UUIDs for Heart Rate service and characteristic.
const (
    HeartRateServiceUUID        = "0000180d-0000-1000-8000-00805f9b34fb"
    HeartRateCharacteristicUUID = "00002a37-0000-1000-8000-00805f9b34fb"
)

var log = logrus.StandardLogger()

func init() {
    logrus.SetFormatter(&logrus.TextFormatter{
        FullTimestamp:   true,
        TimestampFormat: "2006-01-02 15:04:05",
    })
}

// ConnectionState represents the connection state of the heart rate monitor.
type ConnectionState int

const (
    Disconnected ConnectionState = iota
    Connecting
    Connected
    Subscribing
    Subscribed
    Disconnecting
)

func (s ConnectionState) String() string {
    return [...]string{"Disconnected", "Connecting", "Connected", "Subscribing", "Subscribed", "Disconnecting"}[s]
}

// HeartRateMonitor represents a heart rate monitor instance.
type HeartRateMonitor struct {
    config            Config                // Configuration for the monitor
    dataStream        chan HeartRatePayload // Channel to send heart rate data to consumers
    stopSignal        chan struct{}         // Signal to stop monitoring
    reconnectAttempts int                   // Number of reconnect attempts
    debounceDuration  time.Duration         // Duration for debouncing before reconnection attempts
    mu                sync.Mutex            // Mutex for state synchronization
    state             ConnectionState       // Current connection state
    lastDisconnect    time.Time             // Timestamp of last disconnect
    lastDataReceived  time.Time             // Timestamp of last data reception
    sessionLock       sync.Mutex            // Mutex for session management
    peer              *bluetooth.Device     // Bluetooth device representing the connected peer
    reconnectTimer    *time.Timer           // Timer for reconnecting after disconnection
}

// NewHeartRateMonitor creates a new HeartRateMonitor instance.
func NewHeartRateMonitor(config Config) *HeartRateMonitor {
    return &HeartRateMonitor{
        config:           config,
        dataStream:       make(chan HeartRatePayload),
        stopSignal:       make(chan struct{}),
        reconnectAttempts: 3,
        debounceDuration: 5 * time.Second,
        state:            Disconnected,
        lastDataReceived: time.Now(),
    }
}

// Start starts monitoring heart rate.
func (hrm *HeartRateMonitor) Start() {
    go hrm.monitor()
}

// Stop stops monitoring heart rate.
func (hrm *HeartRateMonitor) Stop() {
    hrm.mu.Lock()
    defer hrm.mu.Unlock()
    hrm.setState(Disconnecting)
    close(hrm.stopSignal)
    if hrm.peer != nil {
        hrm.peer.Disconnect()
        hrm.peer = nil
    }
    if hrm.reconnectTimer != nil {
        hrm.reconnectTimer.Stop()
    }
    hrm.setState(Disconnected)
    close(hrm.dataStream)
}

// Subscribe returns a channel to receive heart rate data.
func (hrm *HeartRateMonitor) Subscribe() <-chan HeartRatePayload {
    return hrm.dataStream
}

// setState sets the connection state.
func (hrm *HeartRateMonitor) setState(newState ConnectionState) {
    hrm.state = newState
}

// monitor continuously runs the heart rate monitoring process.
func (hrm *HeartRateMonitor) monitor() {
    for {
        select {
        case <-hrm.stopSignal:
            return
        default:
            hrm.run()
            time.Sleep(hrm.debounceDuration)
        }
    }
}

// run executes the heart rate monitoring process.
func (hrm *HeartRateMonitor) run() {
    hrm.mu.Lock()
    if hrm.state != Disconnected {
        hrm.mu.Unlock()
        return
    }
    hrm.mu.Unlock()

    hrm.setState(Connecting)
    device, err := hrm.scanAndConnect()
    if err != nil {
        log.Errorf("Failed to scan and connect: %v", err)
        hrm.setState(Disconnected)
        return
    }

    hrm.setState(Connected)

    disconnect := func() {
        device.Disconnect()
        hrm.setState(Disconnected)
    }

    services, err := hrm.discoverServices(device)
    if err != nil {
        log.Errorf("Failed to discover services: %v", err)
        disconnect()
        return
    }

    characteristics, err := hrm.discoverCharacteristics(services[0])
    if err != nil {
        log.Errorf("Failed to discover characteristics: %v", err)
        disconnect()
        return
    }

    hrm.setState(Subscribing)
    err = hrm.subscribeHeartRateData(characteristics[0])
    if err != nil {
        log.Errorf("Failed to subscribe to heart rate data: %v", err)
        disconnect()
        return
    }

    hrm.mu.Lock()
    hrm.setState(Subscribed)
    hrm.peer = device
    hrm.mu.Unlock()
}

// scanAndConnect scans for devices and connects to the target device.
func (hrm *HeartRateMonitor) scanAndConnect() (*bluetooth.Device, error) {
    hrm.sessionLock.Lock()
    defer hrm.sessionLock.Unlock()

    adapter := bluetooth.DefaultAdapter

    if err := adapter.Enable(); err != nil {
        return nil, wrapError(err, "enable BLE stack")
    }

    log.Infof("Scanning for %s...", hrm.config.TargetDeviceName)
    ch := make(chan bluetooth.ScanResult, 1)
    go func() {
        err := adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
            if matchesTargetDevice(result, hrm.config) {
                select {
                case ch <- result:
                default:
                }
            }
        })
        if err != nil {
            log.Errorf("scan error: %v", err)
        }
    }()

    var device bluetooth.ScanResult
    select {
    case device = <-ch:
    case <-time.After(time.Duration(hrm.config.ScanTimeout) * time.Second):
        adapter.StopScan()
        return nil, errors.New("timeout while scanning for devices")
    }

    if err := adapter.StopScan(); err != nil {
        return nil, wrapError(err, "stop scan")
    }

    log.Infof("Connecting to %s (%s)...", device.LocalName(), device.Address.String())
    var peer *bluetooth.Device
    for i := 0; i < hrm.reconnectAttempts; i++ {
        p, err := adapter.Connect(device.Address, bluetooth.ConnectionParams{})
        if err == nil {
            peer = &p
            break
        }
        log.Errorf("Connect attempt %d/%d failed: %v", i+1, hrm.reconnectAttempts, err)
        time.Sleep(2 * time.Second)
    }
    if peer == nil {
        return nil, errors.New("failed to connect after multiple attempts")
    }
    return peer, nil
}

// discoverServices discovers services provided by the device.
func (hrm *HeartRateMonitor) discoverServices(peer *bluetooth.Device) ([]bluetooth.DeviceService, error) {
    serviceUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateServiceUUID))
    services, err := peer.DiscoverServices([]bluetooth.UUID{serviceUUID})
    if err != nil {
        return nil, wrapError(err, "discover services")
    }
    if len(services) == 0 {
        return nil, errors.New("no services found")
    }
    return services, nil
}

// discoverCharacteristics discovers characteristics of a service.
func (hrm *HeartRateMonitor) discoverCharacteristics(service bluetooth.DeviceService) ([]bluetooth.DeviceCharacteristic, error) {
    characteristicUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateCharacteristicUUID))
    characteristics, err := service.DiscoverCharacteristics([]bluetooth.UUID{characteristicUUID})
    if err != nil {
        return nil, wrapError(err, "discover characteristics")
    }
    if len(characteristics) == 0 {
        return nil, errors.New("no characteristics found")
    }
    return characteristics, nil
}

// subscribeHeartRateData subscribes to heart rate data notifications.
func (hrm *HeartRateMonitor) subscribeHeartRateData(characteristic bluetooth.DeviceCharacteristic) error {
    dataReceived := make(chan struct{})

    // Start a goroutine to monitor the timeout
    go func() {
        for {
            select {
            case <-time.After(5 * time.Second):
                hrm.mu.Lock()
                if time.Since(hrm.lastDataReceived) > 5*time.Second {
                    log.Warn("No data received for 5s, reconnecting...")

                    // Stop the connection (this implicitly stops notifications)
                    if hrm.peer != nil {
                        hrm.peer.Disconnect()
                        hrm.peer = nil
                    }

                    hrm.setState(Disconnecting)
                    hrm.lastDisconnect = time.Now()
                    hrm.setState(Disconnected)
                    hrm.mu.Unlock()

                    return
                }
                hrm.mu.Unlock()
            case <-dataReceived:
                // Data received, reset the timeout
                hrm.mu.Lock()
                hrm.lastDataReceived = time.Now()
                hrm.mu.Unlock()
            case <-hrm.stopSignal:
                return
            }
        }
    }()

    err := characteristic.EnableNotifications(func(buf []byte) {
        if len(buf) > 0 {
            hr := buf[1]
            payload := HeartRatePayload{
                HeartRate: int(hr),
                Timestamp: time.Now().UTC(),
            }
            hrm.dataStream <- payload
            dataReceived <- struct{}{}
        }
    })

    if err != nil {
        return wrapError(err, "subscribe to heart rate data")
    }
    log.Infof("Streaming heart rate from %s", hrm.config.TargetDeviceName)
    return nil
}

// matchesTargetDevice checks if the device matches the target device name.
func matchesTargetDevice(result bluetooth.ScanResult, config Config) bool {
    if config.TargetDeviceName != "" {
        return strings.Contains(result.LocalName(), config.TargetDeviceName)
    }
    return true
}

// uuidToByteArray converts UUID string to byte array.
func uuidToByteArray(uuid string) [16]byte {
    var ba [16]byte
    b, err := hex.DecodeString(uuid[:8] + uuid[9:13] + uuid[14:18] + uuid[19:23] + uuid[24:])
    if err != nil {
        log.Errorf("Invalid UUID format: %v", err)
        return ba
    }
    copy(ba[:], b)
    return ba
}

// wrapError wraps an error with context.
func wrapError(err error, context string) error {
    return errors.New(context + ": " + err.Error())
}
