package heartrate

import (
    "encoding/binary"
    "encoding/hex"
    "errors"
    "fmt"
    "slices"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "github.com/sirupsen/logrus"
    "tinygo.org/x/bluetooth"
)

// HeartRatePayload represents a heart rate measurement notification.
type HeartRatePayload struct {
    HeartRate   int       `json:"heart_rate"`
    RRIntervals []int     `json:"rr_intervals,omitempty"` // milliseconds
    Timestamp   time.Time `json:"timestamp"`
}

const (
    HeartRateServiceUUID        = "0000180d-0000-1000-8000-00805f9b34fb"
    HeartRateCharacteristicUUID = "00002a37-0000-1000-8000-00805f9b34fb"
    BatteryServiceUUID          = "0000180f-0000-1000-8000-00805f9b34fb"
    BatteryLevelUUID            = "00002a19-0000-1000-8000-00805f9b34fb"
)

var log = logrus.StandardLogger()

func init() {
    logrus.SetFormatter(&logrus.TextFormatter{
        FullTimestamp:   true,
        TimestampFormat: "2006-01-02 15:04:05",
    })
}

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

// validTransitions defines the allowed state machine transitions.
var validTransitions = map[ConnectionState][]ConnectionState{
    Disconnected:  {Connecting, Disconnecting},
    Connecting:    {Connected, Disconnected, Disconnecting},
    Connected:     {Subscribing, Disconnected, Disconnecting},
    Subscribing:   {Subscribed, Disconnected, Disconnecting},
    Subscribed:    {Disconnecting},
    Disconnecting: {Disconnected},
}

// HeartRateMonitor represents a heart rate monitor instance.
type HeartRateMonitor struct {
    config            Config
    dataStream        chan HeartRatePayload
    stopSignal        chan struct{}
    reconnectAttempts int
    debounceDuration  time.Duration
    mu                sync.Mutex
    state             ConnectionState
    lastDisconnect    time.Time
    lastDataReceived  time.Time
    sessionLock       sync.Mutex
    peer              *bluetooth.Device
    reconnectTimer    *time.Timer
    subscriptionGen   uint32 // incremented on each new subscription to invalidate stale callbacks
}

// NewHeartRateMonitor creates a new HeartRateMonitor instance.
func NewHeartRateMonitor(config Config) *HeartRateMonitor {
    return &HeartRateMonitor{
        config:            config,
        dataStream:        make(chan HeartRatePayload),
        stopSignal:        make(chan struct{}),
        reconnectAttempts: 3,
        debounceDuration:  5 * time.Second,
        state:             Disconnected,
        lastDataReceived:  time.Now(),
    }
}

// Start starts monitoring heart rate.
func (hrm *HeartRateMonitor) Start() {
    go hrm.monitor()
}

// Stop stops monitoring heart rate.
func (hrm *HeartRateMonitor) Stop() {
    hrm.mu.Lock()
    if err := hrm.transition(Disconnecting); err != nil {
        hrm.mu.Unlock()
        return
    }
    close(hrm.stopSignal)
    if hrm.peer != nil {
        hrm.peer.Disconnect()
        hrm.peer = nil
    }
    if hrm.reconnectTimer != nil {
        hrm.reconnectTimer.Stop()
    }
    hrm.transition(Disconnected)
    hrm.mu.Unlock()
    close(hrm.dataStream)
}

// Subscribe returns a channel to receive heart rate data.
func (hrm *HeartRateMonitor) Subscribe() <-chan HeartRatePayload {
    return hrm.dataStream
}

// transition validates and applies a state change.
// Must be called with hrm.mu held.
func (hrm *HeartRateMonitor) transition(to ConnectionState) error {
    if !slices.Contains(validTransitions[hrm.state], to) {
        return fmt.Errorf("invalid transition: %s → %s", hrm.state, to)
    }
    hrm.state = to
    return nil
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

// run executes one connection attempt: scan → connect → subscribe.
func (hrm *HeartRateMonitor) run() {
    hrm.mu.Lock()
    err := hrm.transition(Connecting)
    hrm.mu.Unlock()
    if err != nil {
        return // not in Disconnected state, skip
    }

    device, err := hrm.scanAndConnect()
    if err != nil {
        log.Errorf("Failed to scan and connect: %v", err)
        hrm.mu.Lock()
        hrm.transition(Disconnected)
        hrm.mu.Unlock()
        return
    }

    hrm.mu.Lock()
    if err := hrm.transition(Connected); err != nil {
        hrm.mu.Unlock()
        device.Disconnect()
        return
    }
    hrm.mu.Unlock()

    disconnect := func() {
        device.Disconnect()
        hrm.mu.Lock()
        hrm.transition(Disconnected)
        hrm.mu.Unlock()
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

    hrm.readBattery(device)

    hrm.mu.Lock()
    if err := hrm.transition(Subscribing); err != nil {
        hrm.mu.Unlock()
        disconnect()
        return
    }
    hrm.mu.Unlock()

    if err := hrm.subscribeHeartRateData(characteristics[0]); err != nil {
        log.Errorf("Failed to subscribe to heart rate data: %v", err)
        disconnect()
        return
    }

    hrm.mu.Lock()
    if err := hrm.transition(Subscribed); err != nil {
        hrm.mu.Unlock()
        disconnect()
        return
    }
    hrm.peer = device
    hrm.mu.Unlock()
}

// scanAndConnect scans for the target device and connects to it.
func (hrm *HeartRateMonitor) scanAndConnect() (*bluetooth.Device, error) {
    hrm.sessionLock.Lock()
    defer hrm.sessionLock.Unlock()

    adapter := bluetooth.DefaultAdapter

    if err := adapter.Enable(); err != nil {
        return nil, wrapError(err, "enable BLE stack")
    }

    _ = adapter.StopScan()           // clear any stale scan from a previous session
    time.Sleep(500 * time.Millisecond) // give BlueZ time to process the stop

    log.Infof("Scanning for %s...", hrm.config.TargetDeviceName)
    ch := make(chan bluetooth.ScanResult, 1)
    scanDone := make(chan error, 1)
    go func() {
        scanDone <- adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
            if matchesTargetDevice(result, hrm.config) {
                select {
                case ch <- result:
                default:
                }
            }
        })
    }()

    var device bluetooth.ScanResult
    select {
    case device = <-ch:
        adapter.StopScan()
        <-scanDone // wait for the goroutine to exit before proceeding
    case err := <-scanDone:
        if err != nil {
            return nil, wrapError(err, "scan")
        }
        return nil, errors.New("scan ended without finding device")
    case <-time.After(time.Duration(hrm.config.ScanTimeout) * time.Second):
        adapter.StopScan()
        <-scanDone // wait for the goroutine to exit before returning
        return nil, errors.New("timeout while scanning for devices")
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

// discoverServices discovers the heart rate service on the device.
func (hrm *HeartRateMonitor) discoverServices(peer *bluetooth.Device) ([]bluetooth.DeviceService, error) {
    serviceUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateServiceUUID))
    var services []bluetooth.DeviceService
    var err error
    for i := 0; i < 3; i++ {
        services, err = peer.DiscoverServices([]bluetooth.UUID{serviceUUID})
        if err == nil && len(services) > 0 {
            return services, nil
        }
        if i < 2 {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return nil, wrapError(err, "discover services")
    }
    return nil, errors.New("no services found")
}

// discoverCharacteristics discovers the heart rate characteristic on the service.
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

// subscribeHeartRateData enables notifications and watches for data timeouts.
func (hrm *HeartRateMonitor) subscribeHeartRateData(characteristic bluetooth.DeviceCharacteristic) error {
    gen := atomic.AddUint32(&hrm.subscriptionGen, 1)

    hrm.mu.Lock()
    hrm.lastDataReceived = time.Now()
    hrm.mu.Unlock()

    dataReceived := make(chan struct{}, 1)
    batteryTicker := time.NewTicker(60 * time.Second)

    go func() {
        defer batteryTicker.Stop()
        for {
            select {
            case <-batteryTicker.C:
                hrm.mu.Lock()
                device := hrm.peer
                hrm.mu.Unlock()
                if device != nil {
                    hrm.readBattery(device)
                }

            case <-time.After(5 * time.Second):
                hrm.mu.Lock()
                stale := time.Since(hrm.lastDataReceived) > 5*time.Second
                hrm.mu.Unlock()

                if !stale {
                    continue
                }

                log.Warn("No data received for 5s, reconnecting...")
                hrm.mu.Lock()
                if hrm.peer != nil {
                    hrm.peer.Disconnect()
                    hrm.peer = nil
                }
                if err := hrm.transition(Disconnecting); err != nil {
                    hrm.mu.Unlock()
                    return
                }
                hrm.lastDisconnect = time.Now()
                hrm.transition(Disconnected)
                hrm.mu.Unlock()
                return

            case <-dataReceived:
                hrm.mu.Lock()
                hrm.lastDataReceived = time.Now()
                hrm.mu.Unlock()

            case <-hrm.stopSignal:
                return
            }
        }
    }()

    err := characteristic.EnableNotifications(func(buf []byte) {
        if atomic.LoadUint32(&hrm.subscriptionGen) != gen {
            return // stale callback from a previous connection, discard
        }
        if len(buf) < 2 {
            return
        }

        flags := buf[0]

        // Bits 1-2: sensor contact status.
        // 0b10 = supported but not detected → drop the reading.
        contactBits := (flags >> 1) & 0x03
        if contactBits == 0x02 {
            select {
            case dataReceived <- struct{}{}:
            default:
            }
            return
        }

        // Bit 0: HR value format (0 = uint8, 1 = uint16).
        offset := 1
        var hr int
        if flags&0x01 == 0 {
            hr = int(buf[offset])
            offset++
        } else {
            if len(buf) < offset+2 {
                return
            }
            hr = int(binary.LittleEndian.Uint16(buf[offset:]))
            offset += 2
        }

        // H10 reports contactBits=0b00 ("not supported") rather than 0b10 when the
        // strap is removed, so 0 bpm slips through the contact check above. Drop it
        // without updating lastDataReceived so the 5s watchdog triggers a reconnect.
        if hr == 0 {
            return
        }

        // Bit 3: energy expended present (skip 2 bytes).
        if flags&0x08 != 0 {
            offset += 2
        }

        // Bit 4: RR intervals present (each 2 bytes, units = 1/1024 s).
        var rrIntervals []int
        if flags&0x10 != 0 {
            for offset+1 < len(buf) {
                raw := int(binary.LittleEndian.Uint16(buf[offset:]))
                rrIntervals = append(rrIntervals, raw*1000/1024)
                offset += 2
            }
        }

        hrm.dataStream <- HeartRatePayload{
            HeartRate:   hr,
            RRIntervals: rrIntervals,
            Timestamp:   time.Now().UTC(),
        }
        select {
        case dataReceived <- struct{}{}:
        default:
        }
    })
    if err != nil {
        return wrapError(err, "enable notifications")
    }
    log.Infof("Streaming heart rate from %s", hrm.config.TargetDeviceName)
    return nil
}

// readBattery reads and logs the battery level from the device.
func (hrm *HeartRateMonitor) readBattery(device *bluetooth.Device) {
    services, err := device.DiscoverServices([]bluetooth.UUID{
        bluetooth.NewUUID(uuidToByteArray(BatteryServiceUUID)),
    })
    if err != nil {
        log.Warnf("Battery service discovery failed: %v", err)
        return
    }
    if len(services) == 0 {
        log.Warn("Battery service not found")
        return
    }
    chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{
        bluetooth.NewUUID(uuidToByteArray(BatteryLevelUUID)),
    })
    if err != nil {
        log.Warnf("Battery characteristic discovery failed: %v", err)
        return
    }
    if len(chars) == 0 {
        log.Warn("Battery characteristic not found")
        return
    }
    buf := make([]byte, 1)
    n, err := chars[0].Read(buf)
    if err != nil {
        log.Warnf("Battery read failed: %v", err)
        return
    }
    if n == 0 {
        return
    }
    log.Infof("Battery: %d%%", buf[0])
}

// matchesTargetDevice checks if a scan result matches the configured target.
func matchesTargetDevice(result bluetooth.ScanResult, config Config) bool {
    if config.TargetDeviceName != "" {
        return strings.Contains(result.LocalName(), config.TargetDeviceName)
    }
    return true
}

// uuidToByteArray converts a UUID string to a [16]byte array.
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

func wrapError(err error, context string) error {
    return errors.New(context + ": " + err.Error())
}
