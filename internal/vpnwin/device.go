//go:build windows

// Package vpnwin is what "deskconn vpn connect" needs from Windows: a Wintun device and the routes
// and DNS that send this machine's traffic into it. Everything here needs elevation.
package vpnwin

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

const (
	wintunDLLName    = "wintun.dll"
	wintunTunnelType = "Deskconn"

	// ringCapacity is the size of each of the session's packet rings: 4 MiB, the same that
	// WireGuard for Windows uses.
	ringCapacity = 0x400000

	// readWaitMillis is how often Read checks whether the device has closed while it's idle.
	readWaitMillis = 250

	// tunMetric makes the tunnel the first choice for everything routed through it, and its
	// resolvers the first ones Windows asks.
	tunMetric = "1"

	addressReadyTimeout = 10 * time.Second
	addressPollInterval = 100 * time.Millisecond
)

// ErrClosed is what Read returns once the device has closed.
var ErrClosed = errors.New("tunnel device closed")

// Device is a Wintun adapter and its session. Everything set on the adapter, routes included, goes
// away with it, also if this process dies. The host routes PinHostRoute adds are on the physical
// interface instead, so a crash leaves them behind: harmless /32 routes via the usual gateway.
type Device struct {
	adapter *wintun.Adapter
	session wintun.Session
	// readEvent is kept from the start, as asking the session for it isn't safe once it's ended.
	readEvent windows.Handle

	Name  string
	index uint32

	// mu is held for reading while the session is in use, as it can't be ended under a user.
	mu     sync.RWMutex
	closed bool
	done   chan struct{}

	hostRoutes map[string]windows.MibIpForwardRow2
}

// CheckDLL says what's wrong if wintun.dll is missing, as the library panics on it otherwise. It
// only loads the DLL from next to this executable or from System32.
func CheckDLL() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), wintunDLLName)); err == nil {
		return nil
	}
	if system, err := windows.GetSystemDirectory(); err == nil {
		if _, err := os.Stat(filepath.Join(system, wintunDLLName)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s not found next to %s; reinstall deskconn to get it, or for a "+
		"source build run \"make build\", which fetches it into bin", wintunDLLName, exe)
}

// Open creates an adapter named name and starts a session on it. An adapter left behind by a
// process that died is removed by Wintun itself, which cleans up orphaned adapters on creation.
func Open(name string) (*Device, error) {
	if err := CheckDLL(); err != nil {
		return nil, err
	}

	adapter, err := wintun.CreateAdapter(name, wintunTunnelType, nil)
	if err != nil {
		return nil, fmt.Errorf("create wintun adapter %q: %w", name, err)
	}
	session, err := adapter.StartSession(ringCapacity)
	if err != nil {
		_ = adapter.Close()
		return nil, fmt.Errorf("start wintun session: %w", err)
	}

	device := &Device{
		adapter:    adapter,
		session:    session,
		readEvent:  session.ReadWaitEvent(),
		done:       make(chan struct{}),
		hostRoutes: map[string]windows.MibIpForwardRow2{},
	}
	if err := device.resolveInterface(); err != nil {
		device.Close()
		return nil, err
	}
	return device, nil
}

func (d *Device) resolveInterface() error {
	luid := d.adapter.LUID()
	var index uint32
	r, _, _ := procConvertInterfaceLuidToIndex.Call(uintptr(unsafe.Pointer(&luid)), uintptr(unsafe.Pointer(&index)))
	if r != 0 {
		return fmt.Errorf("find adapter's interface index: %w", windows.Errno(r))
	}
	iface, err := net.InterfaceByIndex(int(index))
	if err != nil {
		return fmt.Errorf("look up new adapter: %w", err)
	}
	d.index, d.Name = index, iface.Name
	return nil
}

// Read waits for the next IPv4 packet from the system. IPv6 is dropped here: the tunnel only
// carries IPv4, and RouteAll(6) routes IPv6 into this device for exactly this to happen to it.
func (d *Device) Read(buf []byte) (int, error) {
	for {
		n, again, err := d.readOne(buf)
		if !again {
			return n, err
		}
		select {
		case <-d.done:
			return 0, ErrClosed
		default:
		}
		_, _ = windows.WaitForSingleObject(d.readEvent, readWaitMillis)
	}
}

func (d *Device) readOne(buf []byte) (n int, again bool, err error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return 0, false, ErrClosed
	}
	for {
		packet, err := d.session.ReceivePacket()
		if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			return 0, true, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("wintun receive: %w", err)
		}
		if len(packet) == 0 || packet[0]>>4 != 4 {
			d.session.ReleaseReceivePacket(packet)
			continue
		}
		n = copy(buf, packet)
		d.session.ReleaseReceivePacket(packet)
		return n, false, nil
	}
}

// Write hands a packet to the system, dropping it if the ring is full, like a congested link.
func (d *Device) Write(packet []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return ErrClosed
	}
	buf, err := d.session.AllocateSendPacket(len(packet))
	if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("wintun send: %w", err)
	}
	copy(buf, packet)
	d.session.SendPacket(buf)
	return nil
}

// Close removes what the device added outside its adapter, then the adapter with the rest.
func (d *Device) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	close(d.done)
	d.removeHostRoutes()
	d.session.End()
	_ = d.adapter.Close()
}

func runNetCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // a fixed argv, no shell
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Configure assigns cidr to the device and sets its MTU and metric, then waits for the address to
// be usable. Everything is store=active, so it goes with the adapter instead of persisting.
func (d *Device) Configure(cidr string, mtu int) error {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", cidr, err)
	}

	// A point-to-point tunnel has nobody to detect a duplicate address on or a router to discover,
	// and waiting for the former would leave the address unusable for its first few seconds.
	iface := "interface=" + d.Name
	for _, args := range [][]string{
		{"interface", "ipv4", "set", "interface", iface, "dadtransmits=0", "routerdiscovery=disabled", "store=active"},
		{
			"interface", "ipv4", "set", "address", "name=" + d.Name, "source=static", "address=" + ip.String(),
			"mask=" + net.IP(ipNet.Mask).String(), "gateway=none", "store=active",
		},
		{"interface", "ipv4", "set", "subinterface", d.Name, "mtu=" + strconv.Itoa(mtu), "store=active"},
		{"interface", "ipv4", "set", "interface", iface, "metric=" + tunMetric, "store=active"},
	} {
		if err := runNetCmd("netsh", args...); err != nil {
			return err
		}
	}

	// Windows applies an address asynchronously, and it's listed while still tentative, so only
	// binding to it tells when it's usable.
	deadline := time.Now().Add(addressReadyTimeout)
	for {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to come up on %s", ip, d.Name)
		}
		time.Sleep(addressPollInterval)
	}
}

// SetDNS makes servers the device's resolvers. Windows asks the lowest-metric adapter's first,
// which is this one, but unlike a "~." routing domain on Linux that isn't exclusive: its smart
// multi-homed name resolution may also ask other adapters'. It goes with the adapter.
func (d *Device) SetDNS(servers []string) error {
	if len(servers) == 0 {
		return errors.New("no dns servers given")
	}
	for _, s := range servers {
		if net.ParseIP(s) == nil {
			return fmt.Errorf("invalid dns server %q", s)
		}
	}
	if err := runNetCmd("netsh", "interface", "ipv4", "set", "dnsservers", "name="+d.Name, "source=static",
		"address="+servers[0], "register=none", "validate=no"); err != nil {
		return err
	}
	for i, server := range servers[1:] {
		if err := runNetCmd("netsh", "interface", "ipv4", "add", "dnsservers", "name="+d.Name,
			"address="+server, "index="+strconv.Itoa(i+2), "validate=no"); err != nil {
			return err
		}
	}
	return nil
}
