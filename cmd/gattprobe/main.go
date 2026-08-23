// gattprobe: connect to a BLE device and subscribe to the Heart Rate Measurement
// characteristic (0x2A37) directly over BlueZ D-Bus.
//
// Diagnostic: takes both bluetoothctl and tinygo/bluetooth out of the picture, so
// a failure here is the peripheral's policy, not our client's.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const hrChar = "00002a37-0000-1000-8000-00805f9b34fb"

func main() {
	addr := os.Getenv("MAC")
	if addr == "" {
		fmt.Println("set MAC=XX:XX:...")
		os.Exit(2)
	}
	conn, err := dbus.SystemBus()
	must(err, "system bus")

	devPath, ok := findDevice(conn, addr)
	if !ok {
		fmt.Println("device not known to BlueZ — is it advertising?")
		os.Exit(1)
	}
	fmt.Println("device:", devPath)
	dev := conn.Object("org.bluez", devPath)

	if !boolProp(dev, "org.bluez.Device1", "Connected") {
		fmt.Println("connecting...")
		if call := dev.Call("org.bluez.Device1.Connect", 0); call.Err != nil {
			fmt.Println("connect failed:", call.Err)
			os.Exit(1)
		}
	}
	fmt.Println("connected:", boolProp(dev, "org.bluez.Device1", "Connected"),
		"| paired:", boolProp(dev, "org.bluez.Device1", "Paired"))

	for i := 0; i < 40 && !boolProp(dev, "org.bluez.Device1", "ServicesResolved"); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("services resolved:", boolProp(dev, "org.bluez.Device1", "ServicesResolved"))

	charPath, flags, ok := findChar(conn, devPath, hrChar)
	if !ok {
		fmt.Println("0x2A37 not found — services currently exposed:")
		for path, ifaces := range managed(conn) {
			if !strings.HasPrefix(string(path), string(devPath)) {
				continue
			}
			if svc, ok := ifaces["org.bluez.GattService1"]; ok {
				u, _ := svc["UUID"].Value().(string)
				fmt.Println("  service", u)
			}
			if c, ok := ifaces["org.bluez.GattCharacteristic1"]; ok {
				u, _ := c["UUID"].Value().(string)
				fl, _ := c["Flags"].Value().([]string)
				fmt.Println("    char", u, fl)
			}
		}
		os.Exit(1)
	}
	fmt.Println("char:", charPath)
	fmt.Println("flags:", strings.Join(flags, ","))

	// The question this whole exercise turns on: will BlueZ let us subscribe
	// without a bond, or does the peripheral demand encryption first?
	listen := 25 * time.Second
	if v := os.Getenv("LISTEN_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			listen = time.Duration(n) * time.Second
		}
	}
	ch := conn.Object("org.bluez", charPath)
	if call := ch.Call("org.bluez.GattCharacteristic1.StartNotify", 0); call.Err != nil {
		fmt.Println("StartNotify FAILED:", call.Err)
		os.Exit(1)
	}
	fmt.Printf("StartNotify OK — listening %s for beats\n", listen)

	must(conn.AddMatchSignal(
		dbus.WithMatchObjectPath(charPath),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
	), "add match")
	sig := make(chan *dbus.Signal, 16)
	conn.Signal(sig)
	deadline := time.After(listen)
	n := 0
	for {
		select {
		case s := <-sig:
			if len(s.Body) < 2 {
				continue
			}
			props, _ := s.Body[1].(map[string]dbus.Variant)
			v, ok := props["Value"]
			if !ok {
				continue
			}
			b, _ := v.Value().([]byte)
			if len(b) < 2 {
				continue
			}
			n++
			fmt.Printf("  beat: % x  -> hr=%d bpm\n", b, hrFrom(b))
		case <-deadline:
			fmt.Printf("done — %d notifications in %s\n", n, listen)
			_ = ch.Call("org.bluez.GattCharacteristic1.StopNotify", 0).Err
			return
		}
	}
}

func hrFrom(b []byte) int {
	if b[0]&0x01 == 0 {
		return int(b[1])
	}
	return int(b[1]) | int(b[2])<<8
}

func findDevice(conn *dbus.Conn, addr string) (dbus.ObjectPath, bool) {
	for path, ifaces := range managed(conn) {
		if d, ok := ifaces["org.bluez.Device1"]; ok {
			if a, _ := d["Address"].Value().(string); strings.EqualFold(a, addr) {
				return path, true
			}
		}
	}
	return "", false
}

func findChar(conn *dbus.Conn, dev dbus.ObjectPath, uuid string) (dbus.ObjectPath, []string, bool) {
	for path, ifaces := range managed(conn) {
		c, ok := ifaces["org.bluez.GattCharacteristic1"]
		if !ok || !strings.HasPrefix(string(path), string(dev)) {
			continue
		}
		if u, _ := c["UUID"].Value().(string); strings.EqualFold(u, uuid) {
			flags, _ := c["Flags"].Value().([]string)
			return path, flags, true
		}
	}
	return "", nil, false
}

func managed(conn *dbus.Conn) map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	_ = conn.Object("org.bluez", "/").Call(
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs)
	return objs
}

func boolProp(o dbus.BusObject, iface, name string) bool {
	v, err := o.GetProperty(iface + "." + name)
	if err != nil {
		return false
	}
	b, _ := v.Value().(bool)
	return b
}

func must(err error, ctx string) {
	if err != nil {
		fmt.Println(ctx+":", err)
		os.Exit(1)
	}
}
