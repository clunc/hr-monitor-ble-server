package heartrate

import (
    "encoding/hex"
    "errors"
    "os"
    "strings"
    "sync"
    "time"

    "github.com/sirupsen/logrus"
    "tinygo.org/x/bluetooth"
)

// HeartRatePayload represents the heart rate data along with the timestamp.
type HeartRatePayload struct {
    HeartRate int       `json:"heart_rate"` // Heart rate value
    Timestamp time.Time `json:"timestamp"`  // Timestamp when the data was received
}

// UUIDs for Heart Rate service and characteristic.
const (
    HeartRateServiceUUID        = "0000180d-0000-1000-8000-00805f9b34fb"
    HeartRateCharacteristicUUID = "00002a37-0000-1000-8000-00805f9b34fb"
)

var log = logrus.New()

func init() {
    // Initialize logger
    log.Out = os.Stdout
    log.SetLevel(logrus.InfoLevel)
}

// ConnectionState represents the connection state of the heart rate monitor.
type ConnectionState int

const (
    Disconnected ConnectionState = iota
    Connecting
    Connected
    Subscribing
    Subscribed
)

// HeartRateMonitor represents a heart rate monitor instance.
type HeartRateMonitor struct {
    config            Config                // Configuration for the monitor
    dataStream        chan HeartRatePayload // Channel to send heart rate data to consumers
    stopSignal        chan struct{}         // Signal to stop monitoring
    reconnectAttempts int                   // Number of reconnect attempts
    debounceDuration  time.Duration         // Duration for debouncing before reconnection attempts
    mu                sync.Mutex            // Mutex for state synchronization
    state             ConnectionState      // Current connection state
    lastDisconnect    time.Time             // Timestamp of last disconnect
    sessionLock       sync.Mutex            // Mutex for session management
    peer              *bluetooth.Device    // Bluetooth device representing the connected peer
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
    }
}

// Start starts monitoring heart rate.
func (hrm *HeartRateMonitor) Start() {
    go hrm.monitor()
}

// Stop stops monitoring heart rate.
func (hrm *HeartRateMonitor) Stop() {
    close(hrm.stopSignal) // Close the stop signal channel to signal the monitoring goroutine to stop
    hrm.mu.Lock()         // Lock the mutex for state synchronization
    hrm.setState(Disconnected) // Set the state to Disconnected
    if hrm.peer != nil { // If there is a connected peer
        hrm.peer.Disconnect() // Disconnect from the peer
        hrm.peer = nil        // Set the peer reference to nil
    }
    hrm.mu.Unlock() // Unlock the mutex
}

// Subscribe returns a channel to receive heart rate data.
func (hrm *HeartRateMonitor) Subscribe() <-chan HeartRatePayload {
    return hrm.dataStream // Return the data stream channel
}

// setState sets the connection state.
func (hrm *HeartRateMonitor) setState(newState ConnectionState) {
    hrm.state = newState // Set the connection state to the new state
}

// resetReconnectTimer resets the reconnect timer.
func (hrm *HeartRateMonitor) resetReconnectTimer() {
    if hrm.reconnectTimer != nil { // If there is an existing reconnect timer
        hrm.reconnectTimer.Stop() // Stop the timer
    }
    hrm.reconnectTimer = time.AfterFunc(hrm.debounceDuration, func() { // Reset the timer with the debounce duration
        hrm.mu.Lock() // Lock the mutex for state synchronization
        defer hrm.mu.Unlock() // Ensure mutex is unlocked when the function exits
        if hrm.state == Subscribed { // If already subscribed, no need to reconnect
            return // Exit the function
        }
        hrm.setState(Disconnected) // Set the state to Disconnected
        hrm.lastDisconnect = time.Now() // Record the timestamp of disconnection
        if hrm.peer != nil { // If there is a connected peer
            hrm.peer.Disconnect() // Disconnect from the peer
            hrm.peer = nil        // Set the peer reference to nil
        }
        go hrm.run() // Start the monitoring routine
    })
}

// monitor continuously runs the heart rate monitoring process.
func (hrm *HeartRateMonitor) monitor() {
    for {
        select {
        case <-hrm.stopSignal: // If stop signal received
            return // Exit the monitoring routine
        default: // If no stop signal received
            hrm.run() // Run the heart rate monitoring process
            time.Sleep(5 * time.Second) // Wait before attempting to reconnect
        }
    }
}

// run executes the heart rate monitoring process.
func (hrm *HeartRateMonitor) run() {
    hrm.mu.Lock() // Lock the mutex for state synchronization
    if hrm.state != Disconnected { // If not in Disconnected state
        hrm.mu.Unlock() // Unlock the mutex
        return // Exit the function
    }
    hrm.mu.Unlock() // Unlock the mutex

    hrm.setState(Connecting) // Set the state to Connecting
    device, err := hrm.scanAndConnect() // Scan for and connect to the device
    if err != nil { // If error occurred during scanning and connecting
        log.Errorf("Failed to scan and connect: %v", err) // Log the error
        hrm.setState(Disconnected) // Set the state to Disconnected
        return // Exit the function
    }

    hrm.setState(Connected) // Set the state to Connected
    services, err := hrm.discoverServices(device) // Discover services provided by the device
    if err != nil { // If error occurred during service discovery
        log.Errorf("Failed to discover services: %v", err) // Log the error
        hrm.setState(Disconnected) // Set the state to Disconnected
        return // Exit the function
    }

    characteristics, err := hrm.discoverCharacteristics(services[0]) // Discover characteristics of a service
    if err != nil { // If error occurred during characteristic discovery
        log.Errorf("Failed to discover characteristics: %v", err) // Log the error
        hrm.setState(Disconnected) // Set the state to Disconnected
        return // Exit the function
    }

    hrm.setState(Subscribing) // Set the state to Subscribing
    err = hrm.subscribeHeartRateData(characteristics[0]) // Subscribe to heart rate data notifications
    if err != nil { // If error occurred during subscription
        log.Errorf("Failed to subscribe to heart rate data: %v", err) // Log the error
        hrm.setState(Disconnected) // Set the state to Disconnected
        return // Exit the function
    }

    hrm.mu.Lock() // Lock the mutex for state synchronization
    hrm.setState(Subscribed) // Set the state to Subscribed
    hrm.peer = device // Set the peer reference to the connected device
    hrm.mu.Unlock() // Unlock the mutex
}

// scanAndConnect scans for devices and connects to the target device.
func (hrm *HeartRateMonitor) scanAndConnect() (*bluetooth.Device, error) {
    hrm.sessionLock.Lock() // Lock the sessionLock mutex
    defer hrm.sessionLock.Unlock() // Ensure mutex is unlocked when the function exits

    adapter := bluetooth.DefaultAdapter // Get the default Bluetooth adapter

    if err := adapter.Enable(); err != nil { // Enable the Bluetooth adapter
        return nil, wrapError(err, "enable BLE stack") // Return error if enable fails
    }

    ch := make(chan bluetooth.ScanResult, 1) // Create a channel to receive scan results
    stop := make(chan struct{}) // Create a stop channel
    go func() {
        err := adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
            log.Infof("Found device: %s (%s)", result.Address.String(), result.LocalName()) // Log the found device
            if matchesTargetDevice(result, hrm.config) { // Check if the found device matches the target
                ch <- result // Send the result to the channel
                close(stop)  // Close the stop channel to stop scanning
            }
        })
        if err != nil { // If scan fails
            log.Errorf("start scan: %v", err) // Log the error
        }
    }()

    var device bluetooth.ScanResult // Variable to hold the scan result
    select {
    case device = <-ch: // If device found
        log.Infof("Connecting to device: %s (%s)", device.Address.String(), device.LocalName()) // Log the connection attempt
    case <-time.After(time.Duration(hrm.config.ScanTimeout) * time.Second): // If timeout occurs
        return nil, errors.New("timeout while scanning for devices") // Return timeout error
    case <-stop: // If stop signal received
    }

    if err := adapter.StopScan(); err != nil { // Stop scanning
        return nil, wrapError(err, "stop scan") // Return error if stop fails
    }

    var peer *bluetooth.Device // Variable to hold the connected peer
    for i := 0; i < hrm.reconnectAttempts; i++ { // Loop for reconnect attempts
        p, err := adapter.Connect(device.Address, bluetooth.ConnectionParams{}) // Connect to the device
        if err == nil { // If connection successful
            peer = &p // Set the peer reference
            break // Exit the loop
        }
        log.Errorf("Failed to connect to device (attempt %d/%d): %v", i+1, hrm.reconnectAttempts, err) // Log connection attempt failure
        time.Sleep(2 * time.Second) // Wait before retrying
    }
    if peer == nil { // If connection failed after all attempts
        return nil, errors.New("failed to connect to device after multiple attempts") // Return connection failure error
    }
    log.Info("Connected to the device") // Log successful connection
    return peer, nil // Return the connected peer
}

// discoverServices discovers services provided by the device.
func (hrm *HeartRateMonitor) discoverServices(peer *bluetooth.Device) ([]bluetooth.DeviceService, error) {
    log.Info("Discovering services...") // Log service discovery
    serviceUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateServiceUUID)) // Convert service UUID string to UUID
    services, err := peer.DiscoverServices([]bluetooth.UUID{serviceUUID}) // Discover services
    if err != nil { // If service discovery fails
        return nil, wrapError(err, "discover services") // Return error
    }
    if len(services) == 0 { // If no services found
        return nil, errors.New("no services found") // Return no services found error
    }
    log.Info("Services discovered") // Log successful service discovery
    return services, nil // Return the discovered services
}

// discoverCharacteristics discovers characteristics of a service.
func (hrm *HeartRateMonitor) discoverCharacteristics(service bluetooth.DeviceService) ([]bluetooth.DeviceCharacteristic, error) {
    log.Info("Discovering characteristics...") // Log characteristic discovery
    characteristicUUID := bluetooth.NewUUID(uuidToByteArray(HeartRateCharacteristicUUID)) // Convert characteristic UUID string to UUID
    characteristics, err := service.DiscoverCharacteristics([]bluetooth.UUID{characteristicUUID}) // Discover characteristics
    if err != nil { // If characteristic discovery fails
        return nil, wrapError(err, "discover characteristics") // Return error
    }
    if len(characteristics) == 0 { // If no characteristics found
        return nil, errors.New("no characteristics found") // Return no characteristics found error
    }
    log.Info("Characteristics discovered") // Log successful characteristic discovery
    return characteristics, nil // Return the discovered characteristics
}

// subscribeHeartRateData subscribes to heart rate data notifications.
func (hrm *HeartRateMonitor) subscribeHeartRateData(characteristic bluetooth.DeviceCharacteristic) error {
    log.Info("Subscribing to heart rate data...") // Log subscription attempt
    err := characteristic.EnableNotifications(func(buf []byte) { // Enable notifications for heart rate data
        if len(buf) > 0 { // If data received
            hr := buf[1] // Extract heart rate from buffer
            payload := HeartRatePayload{ // Create payload with heart rate and timestamp
                HeartRate: int(hr),            // Heart rate value
                Timestamp: time.Now().UTC(),   // Timestamp when data received
            }
            hrm.dataStream <- payload // Send payload to data stream channel
            log.Infof("Heart rate data sent to channel: %d bpm at %s", payload.HeartRate, payload.Timestamp) // Log data sent
            hrm.resetReconnectTimer() // Reset reconnect timer on data reception
        } else { // If empty notification buffer received
            log.Warn("Received empty notification buffer") // Log warning
        }
    })
    if err != nil { // If subscription fails
        return wrapError(err, "subscribe to heart rate data") // Return error
    }
    log.Info("Subscribed to heart rate data") // Log successful subscription

    // Monitor connection health
    go hrm.monitorConnection(characteristic) // Start monitoring connection health

    return nil // Return no error
}

// monitorConnection continuously monitors the connection health.
func (hrm *HeartRateMonitor) monitorConnection(characteristic bluetooth.DeviceCharacteristic) {
    ticker := time.NewTicker(5 * time.Second) // Create a ticker for periodic checks
    defer ticker.Stop() // Ensure ticker is stopped when function exits

    for {
        select {
        case <-ticker.C: // On ticker tick
            if !hrm.isDeviceConnected(characteristic) { // If device is not connected
                log.Warn("Device disconnected, attempting to reconnect...") // Log disconnection warning
                hrm.mu.Lock() // Lock mutex for state synchronization
                hrm.setState(Disconnected) // Set state to Disconnected
                hrm.lastDisconnect = time.Now() // Record disconnection timestamp
                if hrm.peer != nil { // If there is a connected peer
                    hrm.peer.Disconnect() // Disconnect from the peer
                    hrm.peer = nil // Set peer reference to nil
                }
                hrm.mu.Unlock() // Unlock mutex
                go hrm.run() // Start monitoring routine
                return // Exit function
            }
        case <-hrm.stopSignal: // If stop signal received
            return // Exit function
        }
    }
}

// isDeviceConnected checks if the device is connected.
func (hrm *HeartRateMonitor) isDeviceConnected(characteristic bluetooth.DeviceCharacteristic) bool {
    // Placeholder for actual connection check logic
    // Depending on the BLE library, this might need to be replaced with the proper call
    return hrm.state == Subscribed // Return true if state is Subscribed
}

// matchesTargetDevice checks if the device matches the target device name.
func matchesTargetDevice(result bluetooth.ScanResult, config Config) bool {
    if config.TargetDeviceName != "" { // If target device name is specified in config
        return strings.Contains(result.LocalName(), config.TargetDeviceName) // Check if local name contains the target device name
    }
    return true // Return true if no target device name specified
}

// uuidToByteArray converts UUID string to byte array.
func uuidToByteArray(uuid string) [16]byte {
    var ba [16]byte // Initialize byte array
    b, err := hex.DecodeString(uuid[:8] + uuid[9:13] + uuid[14:18] + uuid[19:23] + uuid[24:]) // Decode UUID string to byte slice
    if err != nil { // If decoding fails
        log.Errorf("Invalid UUID format: %v", err) // Log error
        return ba // Return empty byte array
    }
    copy(ba[:], b) // Copy byte slice to byte array
    return ba // Return byte array
}

// wrapError wraps an error with context.
func wrapError(err error, context string) error {
    return errors.New(context + ": " + err.Error()) // Wrap error with context and return
}
