package heartrate

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/clunc/hr-monitor-ble-server/pkg/heartratepb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
	"tinygo.org/x/bluetooth"
)

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
	dataStream        chan *heartratepb.HeartRateMeasurement
	stopSignal        chan struct{}
	reconnectAttempts int
	debounceDuration  time.Duration
	dataTimeout       time.Duration // how long a silent link is tolerated
	connectDeadline   time.Duration // how long to hunt before giving up
	desiredSince      time.Time
	mu                sync.Mutex
	state             ConnectionState
	lastDisconnect    time.Time
	lastDataReceived  time.Time
	sessionLock       sync.Mutex
	peer              *bluetooth.Device
	reconnectTimer    *time.Timer
	subscriptionGen   uint32 // incremented on each new subscription to invalidate stale callbacks
	lastDeviceAddr    string // MAC of last connected device, used to clear bluetoothd state on reconnect
	activeChar        *bluetooth.DeviceCharacteristic

	// Operator intent + observability for the control API. `desired` is the
	// switch the HTTP layer flips: the supervisor loop leaves the radio alone
	// until it is set, so an idle gateway never holds a discovery session.
	desired     atomic.Bool
	adapterID   string
	adapter     *bluetooth.Adapter
	deviceName  string
	deviceAddr  string
	connectedAt time.Time
	connectedOK bool // last attempt reached the device: don't wipe its object
	gotData     bool // at least one beat arrived on the current subscription
	lastHR      uint32
	lastHRAt    time.Time
	battery     uint8
	source      string
	lastErr     string
	seen        map[string]seenDevice
}

// seenDevice is a scan hit, kept so the UI can offer a device to pick.
type seenDevice struct {
	name string
	rssi int16
	hr   bool // advertises the Heart Rate service — i.e. actually a strap
	at   time.Time
}

// Status is the control API's view of the gateway.
type Status struct {
	Link      string  `json:"link"` // idle | scanning | connected
	Source    string  `json:"source,omitempty"`
	State     string  `json:"state"`
	Desired   bool    `json:"desired"`
	Adapter   string  `json:"adapter"`
	Target    string  `json:"target,omitempty"`
	TargetMAC string  `json:"target_mac,omitempty"`
	Device    string  `json:"device,omitempty"`
	Address   string  `json:"address,omitempty"`
	LastHR    uint32  `json:"last_hr,omitempty"`
	Battery   uint8   `json:"battery,omitempty"`
	Paired    bool    `json:"paired"`
	TargetAge float64 `json:"target_last_seen_s,omitempty"`
	LastHRAge float64 `json:"last_hr_age_s,omitempty"`
	Connected float64 `json:"connected_s,omitempty"`
	LastError string  `json:"last_error,omitempty"`
}

// Device is a scan hit as reported by GET /devices.
type Device struct {
	Address string  `json:"address"`
	Name    string  `json:"name,omitempty"`
	RSSI    int16   `json:"rssi"`
	HR      bool    `json:"hr"`
	AgeS    float64 `json:"age_s"`
}

// ErrBusy is returned by Connect when a link or attempt is already in flight.
var ErrBusy = errors.New("already connecting or connected")

// NewHeartRateMonitor creates a new HeartRateMonitor instance.
func NewHeartRateMonitor(config Config) *HeartRateMonitor {
	return &HeartRateMonitor{
		config:            config,
		dataStream:        make(chan *heartratepb.HeartRateMeasurement),
		stopSignal:        make(chan struct{}),
		reconnectAttempts: 3,
		debounceDuration:  5 * time.Second,
		dataTimeout:       envDuration("HR_DATA_TIMEOUT_SECONDS", 20*time.Second),
		connectDeadline:   envDuration("HR_CONNECT_DEADLINE_SECONDS", 5*time.Minute),
		state:             Disconnected,
		lastDataReceived:  time.Now(),
		seen:              map[string]seenDevice{},
	}
}

// Start launches the supervisor loop. The loop is idle — and the radio
// untouched — until Connect() is called. Auto-connect at boot is the caller's
// choice (AUTO_CONNECT), not this package's default: holding the strap around
// the clock drains it and keeps it out of reach of a watch or phone.
func (hrm *HeartRateMonitor) Start() {
	go hrm.monitor()
}

// SetSource labels this gateway's strap, matching what it stamps on published
// measurements, so peers can be told apart in a combined view.
func (hrm *HeartRateMonitor) SetSource(s string) {
	hrm.mu.Lock()
	hrm.source = s
	hrm.mu.Unlock()
}

// Connect asks the gateway to acquire and hold a link. Optional name/mac
// override the configured target so the UI can pick a device from GET /devices.
// Returns ErrBusy if an attempt or link is already in flight.
func (hrm *HeartRateMonitor) Connect(name, mac string) error {
	if hrm.desired.Swap(true) {
		return ErrBusy
	}
	// matchesTargetDevice ANDs address and name, so a selector must replace the
	// other one rather than accumulate with it. Without this, picking a device by
	// address pinned it permanently: a later ?name= left the stale MAC in place,
	// the two could never both match, and only a restart cleared it.
	hrm.mu.Lock()
	if mac != "" {
		hrm.config.TargetDeviceMAC = mac
		hrm.config.TargetDeviceName = name // "" unless the caller sent both
	} else if name != "" {
		hrm.config.TargetDeviceName = name
		hrm.config.TargetDeviceMAC = ""
	}
	// A new target invalidates the previous strap's reading; keeping it makes the
	// UI show a stale bpm under a "scanning" chip.
	if name != "" || mac != "" {
		hrm.lastHR, hrm.lastHRAt = 0, time.Time{}
	}
	hrm.lastErr = ""
	hrm.desiredSince = time.Now()
	hrm.mu.Unlock()
	return nil
}

// Disconnect drops the link, aborting an in-flight scan, and leaves the radio
// idle until the next Connect. Idempotent.
func (hrm *HeartRateMonitor) Disconnect() {
	hrm.desired.Store(false)
	if a := hrm.currentAdapter(); a != nil {
		_ = a.StopScan() // unblocks a scan attempt parked in scanAndConnect
	}
	hrm.mu.Lock()
	if hrm.activeChar != nil {
		hrm.activeChar.EnableNotifications(nil)
		hrm.activeChar = nil
	}
	peer := hrm.peer
	hrm.peer = nil
	hrm.mu.Unlock()
	if peer != nil {
		peer.Disconnect()
	}
	// Forced teardown bypasses transition(): an operator disconnect must land in
	// Disconnected from any state, including a half-finished attempt.
	hrm.mu.Lock()
	hrm.state = Disconnected
	hrm.deviceName, hrm.deviceAddr = "", ""
	hrm.connectedAt = time.Time{}
	hrm.battery = 0
	hrm.mu.Unlock()
}

// Status reports what the gateway is doing, for GET /status and the dashboard.
func (hrm *HeartRateMonitor) Status() Status {
	hrm.mu.Lock()
	defer hrm.mu.Unlock()
	st := Status{
		State:     hrm.state.String(),
		Source:    hrm.source,
		Desired:   hrm.desired.Load(),
		Adapter:   hrm.adapterID,
		Target:    hrm.config.TargetDeviceName,
		TargetMAC: hrm.config.TargetDeviceMAC,
		Device:    hrm.deviceName,
		Address:   hrm.deviceAddr,
		LastHR:    hrm.lastHR,
		Battery:   hrm.battery,
		Paired:    hrm.deviceAddr != "" && devicePaired(hrm.deviceAddr),
		LastError: hrm.lastErr,
	}
	// Distinguish "hunting for it" from "found it, negotiating". Collapsing both
	// into "scanning" makes a link in progress look idle, which invites a click
	// that tears down the handshake a second before it completes.
	switch {
	case hrm.state == Subscribed && hrm.gotData:
		st.Link = "connected"
	case hrm.state == Subscribed:
		st.Link = "waiting"
	case hrm.state == Connected || hrm.state == Subscribing:
		st.Link = "linking"
	case hrm.state == Connecting && hrm.deviceAddr != "":
		st.Link = "connecting"
	case st.Desired:
		st.Link = "scanning"
	default:
		st.Link = "idle"
	}
	// Age of the most recent advertisement from the configured target, so the
	// page can say "not broadcasting" instead of just showing an empty list.
	for addr, d := range hrm.seen {
		nameMatch := hrm.config.TargetDeviceName != "" && strings.Contains(d.name, hrm.config.TargetDeviceName)
		macMatch := hrm.config.TargetDeviceMAC != "" && strings.EqualFold(addr, hrm.config.TargetDeviceMAC)
		if nameMatch || macMatch {
			if age := time.Since(d.at).Seconds(); st.TargetAge == 0 || age < st.TargetAge {
				st.TargetAge = age
			}
		}
	}
	if !hrm.lastHRAt.IsZero() {
		st.LastHRAge = time.Since(hrm.lastHRAt).Seconds()
	}
	if !hrm.connectedAt.IsZero() {
		st.Connected = time.Since(hrm.connectedAt).Seconds()
	}
	return st
}

// Devices returns scan hits from the last 5 minutes, strongest first.
func (hrm *HeartRateMonitor) Devices() []Device {
	hrm.mu.Lock()
	defer hrm.mu.Unlock()
	out := make([]Device, 0, len(hrm.seen))
	for addr, d := range hrm.seen {
		age := time.Since(d.at)
		if age > 5*time.Minute {
			delete(hrm.seen, addr)
			continue
		}
		out = append(out, Device{Address: addr, Name: d.name, RSSI: d.rssi, HR: d.hr, AgeS: age.Seconds()})
	}
	// Straps first, then strongest signal: in a dense RF environment the list is
	// mostly neighbours' unnamed beacons, and the strap is what you came for.
	slices.SortFunc(out, func(a, b Device) int {
		if a.HR != b.HR {
			if a.HR {
				return -1
			}
			return 1
		}
		return int(b.RSSI) - int(a.RSSI)
	})
	return out
}

// ensureAdapter resolves the BlueZ adapter once and reuses it. NewAdapter returns
// a fresh struct per call, so re-creating it per reconnect would leave an
// un-Enabled adapter (the ble-tape gotcha).
func (hrm *HeartRateMonitor) ensureAdapter() *bluetooth.Adapter {
	hrm.mu.Lock()
	defer hrm.mu.Unlock()
	if hrm.adapter == nil {
		hrm.adapterID = resolveAdapter()
		hrm.adapter = adapterFor(hrm.adapterID)
	}
	return hrm.adapter
}

// subscribeWithTimeout bounds the GATT subscribe. If the peripheral walks out of
// range mid-handshake the underlying call never returns, which wedges the whole
// attempt — and the connect deadline only runs *between* attempts, so nothing
// else would ever notice.
func (hrm *HeartRateMonitor) subscribeWithTimeout(c bluetooth.DeviceCharacteristic) error {
	done := make(chan error, 1)
	go func() { done <- hrm.subscribeHeartRateData(c) }()
	select {
	case err := <-done:
		return err
	case <-time.After(envDuration("HR_SUBSCRIBE_TIMEOUT_SECONDS", 25*time.Second)):
		return errors.New("subscribe timed out (device gone?)")
	}
}

// setErr records a user-facing error for GET /status.
func (hrm *HeartRateMonitor) setErr(msg string) {
	hrm.mu.Lock()
	hrm.lastErr = msg
	hrm.mu.Unlock()
}

func (hrm *HeartRateMonitor) currentAddr() string {
	hrm.mu.Lock()
	defer hrm.mu.Unlock()
	return hrm.deviceAddr
}

func (hrm *HeartRateMonitor) currentAdapter() *bluetooth.Adapter {
	hrm.mu.Lock()
	defer hrm.mu.Unlock()
	return hrm.adapter
}

// Stop stops monitoring heart rate.
func (hrm *HeartRateMonitor) Stop() {
	hrm.mu.Lock()
	if err := hrm.transition(Disconnecting); err != nil {
		hrm.mu.Unlock()
		return
	}
	close(hrm.stopSignal)
	if hrm.activeChar != nil {
		hrm.activeChar.EnableNotifications(nil)
		hrm.activeChar = nil
	}
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
func (hrm *HeartRateMonitor) Subscribe() <-chan *heartratepb.HeartRateMeasurement {
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
		}
		if !hrm.desired.Load() {
			time.Sleep(250 * time.Millisecond) // idle: radio untouched
			continue
		}
		hrm.mu.Lock()
		if hrm.state == Subscribed {
			hrm.desiredSince = time.Now() // healthy link: keep the clock fresh
		}
		expired := hrm.state != Subscribed && !hrm.desiredSince.IsZero() &&
			time.Since(hrm.desiredSince) > hrm.connectDeadline
		deadline := hrm.connectDeadline
		hrm.mu.Unlock()
		if expired {
			log.Warnf("Stopped after %s — device never streamed", deadline)
			hrm.Disconnect()
			hrm.mu.Lock()
			hrm.lastErr = fmt.Sprintf("stopped after %s — device never streamed", deadline)
			hrm.mu.Unlock()
			continue
		}
		hrm.run()
		time.Sleep(hrm.debounceDuration)
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
		hrm.mu.Lock()
		if hrm.desired.Load() {
			// A genuine failure. An operator Disconnect also lands here (StopScan
			// unblocks the scan), but that isn't an error worth showing the user.
			log.Errorf("Failed to scan and connect: %v", err)
			hrm.lastErr = err.Error()
		}
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
		hrm.mu.Lock()
		if hrm.activeChar != nil {
			hrm.activeChar.EnableNotifications(nil)
			hrm.activeChar = nil
		}
		hrm.mu.Unlock()
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

	if err := hrm.subscribeWithTimeout(characteristics[0]); err != nil {
		if !needsPairing(err) {
			log.Errorf("Failed to subscribe to heart rate data: %v", err)
			hrm.setErr(err.Error())
			disconnect()
			return
		}
		// The characteristic needs an encrypted link. Bond, then retry once:
		// devices like a Fitbit refuse notifications until bonded, and there is
		// no desktop pairing agent on the target node to do it for us.
		log.Warn("Heart-rate characteristic requires a bonded link — pairing")
		hrm.setErr("pairing with device…")
		// Long enough for a human to accept a prompt on the device itself; 30s
		// expired while the watch was still waiting to be tapped.
		if perr := pairDevice(hrm.currentAddr(), envDuration("HR_PAIR_TIMEOUT_SECONDS", 28*time.Second)); perr != nil {
			log.Errorf("Pairing failed: %v", perr)
			hrm.setErr("pairing failed: " + perr.Error())
			disconnect()
			return
		}
		if err := hrm.subscribeWithTimeout(characteristics[0]); err != nil {
			log.Errorf("Failed to subscribe after pairing: %v", err)
			hrm.setErr("subscribe after pairing: " + err.Error())
			disconnect()
			return
		}
	}

	hrm.mu.Lock()
	if err := hrm.transition(Subscribed); err != nil {
		hrm.mu.Unlock()
		disconnect()
		return
	}
	hrm.peer = device
	hrm.activeChar = &characteristics[0]
	hrm.connectedAt = time.Now()
	hrm.desiredSince = time.Now()
	hrm.lastErr = ""
	hrm.mu.Unlock()
}

// scanAndConnect scans for the target device and connects to it.
func (hrm *HeartRateMonitor) scanAndConnect() (*bluetooth.Device, error) {
	hrm.sessionLock.Lock()
	defer hrm.sessionLock.Unlock()

	adapter := hrm.ensureAdapter()

	if err := adapter.Enable(); err != nil {
		return nil, wrapError(err, "enable BLE stack")
	}

	// Remove device from bluetoothd cache to stop its background reconnect scan,
	// which holds a discovery session and blocks our StartDiscovery call.
	// Clearing bluetoothd's cache unblocks a stale discovery session, but it also
	// destroys the device object — and with it any bond being negotiated. Only do
	// it when the last attempt never reached the device; if we connected and are
	// mid-pairing, wiping the object throws away the user's approval on the watch.
	hrm.mu.Lock()
	reached := hrm.connectedOK
	hrm.mu.Unlock()
	if hrm.lastDeviceAddr != "" && !reached && !devicePaired(hrm.lastDeviceAddr) {
		_ = exec.Command("bluetoothctl", "remove", hrm.lastDeviceAddr).Run()
		time.Sleep(500 * time.Millisecond)
	}

	_ = adapter.StopScan()

	log.Infof("Scanning for %s...", hrm.config.TargetDeviceName)

	var device bluetooth.ScanResult
	var found bool
	for attempt := range 4 {
		if !hrm.desired.Load() {
			return nil, errors.New("connect aborted")
		}
		if attempt > 0 {
			_ = adapter.StopScan()
			time.Sleep(3 * time.Second)
		}

		ch := make(chan bluetooth.ScanResult, 1)
		scanDone := make(chan error, 1)
		go func() {
			scanDone <- adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
				// Record every hit, not just matches, so the UI can offer a
				// device list when the configured name doesn't show up.
				hrm.mu.Lock()
				hrm.seen[result.Address.String()] = seenDevice{
					name: result.LocalName(),
					rssi: result.RSSI,
					hr:   result.HasServiceUUID(bluetooth.NewUUID(uuidToByteArray(HeartRateServiceUUID))),
					at:   time.Now(),
				}
				hrm.mu.Unlock()
				if matchesTargetDevice(result, hrm.config) {
					select {
					case ch <- result:
					default:
					}
				}
			})
		}()

		select {
		case device = <-ch:
			adapter.StopScan()
			<-scanDone
			hrm.lastDeviceAddr = device.Address.String()
			hrm.mu.Lock()
			hrm.deviceName, hrm.deviceAddr = device.LocalName(), device.Address.String()
			hrm.mu.Unlock()
			found = true
		case err := <-scanDone:
			if err != nil && strings.Contains(err.Error(), "already in progress") {
				log.Warnf("BLE adapter busy, retrying... (%d/4)", attempt+1)
				continue
			}
			if err != nil {
				return nil, wrapError(err, "scan")
			}
			return nil, errors.New("scan ended without finding device")
		case <-time.After(time.Duration(hrm.config.ScanTimeout) * time.Second):
			adapter.StopScan()
			<-scanDone
			hrm.mu.Lock()
			hrm.connectedOK = false
			hrm.mu.Unlock()
			return nil, errors.New("timeout while scanning for devices")
		}
		break
	}
	if !found {
		return nil, errors.New("BLE adapter busy after retries")
	}

	log.Infof("Connecting to %s (%s)...", device.LocalName(), device.Address.String())
	hrm.mu.Lock()
	hrm.connectedOK = true
	hrm.mu.Unlock()
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
	batteryMisses := 0

	startWatchdog := func() {
		go func() {
			defer batteryTicker.Stop()
			for {
				select {
				case <-batteryTicker.C:
					// Some straps (Fitbit) expose no battery service at all;
					// re-probing it every minute just fills the log.
					if batteryMisses >= 3 {
						batteryTicker.Stop()
						continue
					}
					hrm.mu.Lock()
					device := hrm.peer
					had := hrm.battery
					hrm.mu.Unlock()
					if device != nil {
						hrm.readBattery(device)
						hrm.mu.Lock()
						got := hrm.battery
						hrm.mu.Unlock()
						if got == had {
							batteryMisses++
							if batteryMisses == 3 {
								log.Info("battery service unavailable on this device — stopping battery reads")
							}
						} else {
							batteryMisses = 0
						}
					}

				case <-time.After(hrm.dataTimeout):
					hrm.mu.Lock()
					stale := time.Since(hrm.lastDataReceived) > hrm.dataTimeout
					hrm.mu.Unlock()

					if !stale {
						continue
					}
					hrm.mu.Lock()
					never := !hrm.gotData
					hrm.mu.Unlock()
					if never {
						// Subscribed, nothing yet. Do NOT tear down: a Charge 6
						// only starts streaming when "HR on Equipment" is tapped,
						// and the subscription must already be open when it does.
						continue
					}

					log.Warnf("Stream stopped for %s, reconnecting...", hrm.dataTimeout)
					hrm.mu.Lock()
					if hrm.activeChar != nil {
						hrm.activeChar.EnableNotifications(nil)
						hrm.activeChar = nil
					}
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
	}

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
		var rrIntervals []uint32
		if flags&0x10 != 0 {
			for offset+1 < len(buf) {
				raw := int(binary.LittleEndian.Uint16(buf[offset:]))
				rrIntervals = append(rrIntervals, uint32(raw*1000/1024))
				offset += 2
			}
		}

		hrm.mu.Lock()
		hrm.lastHR, hrm.lastHRAt = uint32(hr), time.Now()
		if !hrm.gotData {
			hrm.gotData = true
			log.Info("stream started — beats flowing")
		}
		hrm.mu.Unlock()

		hrm.dataStream <- &heartratepb.HeartRateMeasurement{
			HeartRate:   uint32(hr),
			RrIntervals: rrIntervals,
			Timestamp:   timestamppb.Now(),
		}
		select {
		case dataReceived <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return wrapError(err, "enable notifications")
	}
	// Arm the staleness watchdog only now. Started before this point it races
	// GATT setup, and on a slower device (a Fitbit resolves services in ~5s) it
	// tears the link down mid-subscription, before a single beat can arrive.
	hrm.mu.Lock()
	hrm.lastDataReceived = time.Now()
	hrm.gotData = false
	hrm.mu.Unlock()
	startWatchdog()
	log.Infof("Streaming heart rate from %s", deviceLabel(hrm.config))
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
	hrm.mu.Lock()
	hrm.battery = buf[0]
	hrm.mu.Unlock()
	log.Infof("Battery: %d%%", buf[0])
}

// matchesTargetDevice checks if a scan result matches the configured target.
func matchesTargetDevice(result bluetooth.ScanResult, config Config) bool {
	if config.TargetDeviceMAC != "" && !strings.EqualFold(result.Address.String(), config.TargetDeviceMAC) {
		return false
	}
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

// envDuration reads a seconds-valued env var, falling back to def.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// deviceLabel names the target for logs: the configured name, else the address.
func deviceLabel(c Config) string {
	if c.TargetDeviceName != "" {
		return c.TargetDeviceName
	}
	if c.TargetDeviceMAC != "" {
		return c.TargetDeviceMAC
	}
	return "device"
}
