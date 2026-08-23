package heartrate

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// A gateway that needs a bonded link cannot borrow the desktop's pairing agent:
// on the target node (headless k3s, no session bus, no GNOME) there is none. So
// the gateway registers its own "Just Works" agent — NoInputNoOutput, auto-accept
// — which is the right capability for a device with no keypad or display.
const agentPath = dbus.ObjectPath("/hrmonitor/agent")

type justWorksAgent struct{}

func (a *justWorksAgent) Release() *dbus.Error { return nil }
func (a *justWorksAgent) RequestPinCode(dev dbus.ObjectPath) (string, *dbus.Error) {
	return "0000", nil
}
func (a *justWorksAgent) DisplayPinCode(dev dbus.ObjectPath, pin string) *dbus.Error { return nil }
func (a *justWorksAgent) RequestPasskey(dev dbus.ObjectPath) (uint32, *dbus.Error) {
	return 0, nil
}
func (a *justWorksAgent) DisplayPasskey(dev dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	return nil
}
func (a *justWorksAgent) RequestConfirmation(dev dbus.ObjectPath, passkey uint32) *dbus.Error {
	log.Infof("pairing: auto-confirming passkey %06d", passkey)
	return nil
}
func (a *justWorksAgent) RequestAuthorization(dev dbus.ObjectPath) *dbus.Error { return nil }
func (a *justWorksAgent) AuthorizeService(dev dbus.ObjectPath, uuid string) *dbus.Error {
	return nil
}
func (a *justWorksAgent) Cancel() *dbus.Error { return nil }

var (
	agentReady bool
	// One bonding attempt at a time. Overlapping attempts were fatal: a stale
	// attempt's timeout fired CancelPairing on the *current* one, so an approval
	// tapped on the device got cancelled by our own leftover timer.
	pairingMu sync.Mutex
)

// ensureAgent registers the gateway's pairing agent once. Becoming the *default*
// agent is best-effort: on a desktop GNOME already holds it, and BlueZ still
// routes requests for pairings we initiate ourselves.
func ensureAgent(conn *dbus.Conn) error {
	if agentReady {
		return nil
	}
	if err := conn.Export(&justWorksAgent{}, agentPath, "org.bluez.Agent1"); err != nil {
		return fmt.Errorf("export agent: %w", err)
	}
	mgr := conn.Object("org.bluez", "/org/bluez")
	if call := mgr.Call("org.bluez.AgentManager1.RegisterAgent", 0, agentPath, "NoInputNoOutput"); call.Err != nil {
		if !strings.Contains(call.Err.Error(), "AlreadyExists") {
			return fmt.Errorf("register agent: %w", call.Err)
		}
	}
	if call := mgr.Call("org.bluez.AgentManager1.RequestDefaultAgent", 0, agentPath); call.Err != nil {
		log.Warnf("pairing: not the default agent (%v) — fine unless pairing stalls", call.Err)
	}
	agentReady = true
	log.Info("pairing: agent registered (NoInputNoOutput)")
	return nil
}

// devicePath resolves a BlueZ device object path by address.
func devicePath(conn *dbus.Conn, addr string) (dbus.ObjectPath, bool) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := conn.Object("org.bluez", "/").Call(
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs); err != nil {
		return "", false
	}
	for path, ifaces := range objs {
		dev, ok := ifaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		if a, _ := dev["Address"].Value().(string); strings.EqualFold(a, addr) {
			return path, true
		}
	}
	return "", false
}

// pairDevice bonds with addr, so a characteristic that demands an encrypted link
// can be subscribed to. Returns quickly if the bond already exists.
func pairDevice(addr string, timeout time.Duration) error {
	if !pairingMu.TryLock() {
		return fmt.Errorf("pairing already in progress")
	}
	defer pairingMu.Unlock()
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("system bus: %w", err)
	}
	if err := ensureAgent(conn); err != nil {
		return err
	}
	path, ok := devicePath(conn, addr)
	if !ok {
		return fmt.Errorf("device %s not known to BlueZ", addr)
	}
	dev := conn.Object("org.bluez", path)
	if v, err := dev.GetProperty("org.bluez.Device1.Paired"); err == nil {
		if paired, _ := v.Value().(bool); paired {
			return nil
		}
	}
	log.Infof("pairing: bonding with %s...", addr)
	ch := make(chan *dbus.Call, 1)
	dev.Go("org.bluez.Device1.Pair", 0, ch)
	select {
	case call := <-ch:
		if call.Err != nil {
			return fmt.Errorf("pair: %w", call.Err)
		}
	case <-time.After(timeout):
		// Cancel only our own in-flight attempt, and only inside the window —
		// SMP itself times out at 30s, so a longer wait can never help anyway.
		_ = dev.Call("org.bluez.Device1.CancelPairing", 0).Err
		return fmt.Errorf("pair: no response from device within %s", timeout)
	}
	// Trust it so BlueZ reconnects without re-authorising every time.
	_ = dev.Call("org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.Device1", "Trusted", dbus.MakeVariant(true)).Err
	log.Infof("pairing: bonded with %s", addr)
	return nil
}

// needsPairing reports whether a subscribe error means "this link must be
// encrypted first" rather than something we should give up on.
func needsPairing(err error) bool {
	if err == nil {
		return false
	}
	e := strings.ToLower(err.Error())
	// A hung subscribe counts: some peripherals stall the CCCD write instead of
	// refusing it outright, and bonding is the only thing that unblocks it. The
	// cost of a needless pair attempt is one prompt; the cost of skipping it is
	// never asking at all.
	return strings.Contains(e, "not paired") ||
		strings.Contains(e, "not authorized") ||
		strings.Contains(e, "insufficient authentication") ||
		strings.Contains(e, "insufficient encryption") ||
		strings.Contains(e, "subscribe timed out")
}
