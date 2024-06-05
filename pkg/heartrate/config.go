package heartrate

// Config holds the configuration for the heart rate monitor
type Config struct {
    TargetDeviceMAC  string
    TargetDeviceName string
    ScanTimeout      int
    ReconnectAttempts int // Add this line
}
