package heartrate

import (
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"tinygo.org/x/bluetooth"
)

var (
	resolvedAdapter string
	resolveOnce     sync.Once
)

// resolveAdapter picks the BlueZ adapter id to use. It prefers $BLE_ADAPTER when
// that adapter actually exists, otherwise falls back to the first adapter BlueZ
// reports — so a hci0/hci1 re-enumeration across reboots doesn't break startup.
// Mirrors the same helper in walkingpad-gateway / ble-tape-gateway.
func resolveAdapter() string {
	resolveOnce.Do(func() {
		want := os.Getenv("BLE_ADAPTER")
		adapters := listBluezAdapters()
		switch {
		case len(adapters) == 0:
			if want != "" {
				resolvedAdapter = want
			} else {
				resolvedAdapter = "hci0"
			}
		case contains(adapters, want):
			resolvedAdapter = want
		default:
			resolvedAdapter = adapters[0]
		}
	})
	return resolvedAdapter
}

// adapterFor returns the tinygo adapter for the resolved id. "hci0" maps to
// DefaultAdapter so behaviour is unchanged on single-adapter hosts.
func adapterFor(id string) *bluetooth.Adapter {
	if id == "" || id == "hci0" {
		return bluetooth.DefaultAdapter
	}
	return bluetooth.NewAdapter(id)
}

// devicePaired reports whether BlueZ holds a bond for addr.
//
// Bonded devices must never be `bluetoothctl remove`d: that drops the bond, and
// a device which requires pairing before it will stream (a Fitbit refuses
// notifications with "Not paired") would then fail on every single reconnect,
// with the removal quietly undoing the fix each time.
func devicePaired(addr string) bool {
	bus, err := dbus.SystemBus()
	if err != nil {
		return false
	}
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := bus.Object("org.bluez", "/").Call(
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs); err != nil {
		return false
	}
	for _, ifaces := range objs {
		dev, ok := ifaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		a, _ := dev["Address"].Value().(string)
		if !strings.EqualFold(a, addr) {
			continue
		}
		paired, _ := dev["Paired"].Value().(bool)
		return paired
	}
	return false
}

func listBluezAdapters() []string {
	bus, err := dbus.SystemBus()
	if err != nil {
		return nil
	}
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := bus.Object("org.bluez", "/").Call(
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs); err != nil {
		return nil
	}
	var out []string
	for path, ifaces := range objs {
		if _, ok := ifaces["org.bluez.Adapter1"]; ok {
			p := string(path)
			out = append(out, p[strings.LastIndex(p, "/")+1:])
		}
	}
	sort.Strings(out)
	return out
}

func contains(s []string, v string) bool {
	if v == "" {
		return false
	}
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
