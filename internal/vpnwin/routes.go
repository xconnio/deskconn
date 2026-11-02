//go:build windows

package vpnwin

import (
	"errors"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	sockaddrInet4AddrOffset = 4
	sockaddrInet6AddrOffset = 8
)

//nolint:gochecknoglobals
var (
	modIphlpapi                     = windows.NewLazySystemDLL("iphlpapi.dll")
	procInitializeIpForwardEntry    = modIphlpapi.NewProc("InitializeIpForwardEntry")
	procCreateIpForwardEntry2       = modIphlpapi.NewProc("CreateIpForwardEntry2")
	procDeleteIpForwardEntry2       = modIphlpapi.NewProc("DeleteIpForwardEntry2")
	procConvertInterfaceLuidToIndex = modIphlpapi.NewProc("ConvertInterfaceLuidToIndex")
)

// DefaultRoute is an IPv4 or IPv6 default route. Gateway is nil for an on-link one.
type DefaultRoute struct {
	Index   uint32
	Gateway net.IP
}

// sockaddrIP returns a copy, as addr usually lives in a table that's about to be freed.
func sockaddrIP(addr *windows.RawSockaddrInet) net.IP {
	raw := (*[unsafe.Sizeof(*addr)]byte)(unsafe.Pointer(addr))
	switch addr.Family {
	case windows.AF_INET:
		return append(net.IP(nil), raw[sockaddrInet4AddrOffset:sockaddrInet4AddrOffset+net.IPv4len]...)
	case windows.AF_INET6:
		return append(net.IP(nil), raw[sockaddrInet6AddrOffset:sockaddrInet6AddrOffset+net.IPv6len]...)
	}
	return nil
}

func setSockaddrIP(addr *windows.RawSockaddrInet, ip net.IP) error {
	*addr = windows.RawSockaddrInet{}
	raw := (*[unsafe.Sizeof(*addr)]byte)(unsafe.Pointer(addr))
	if v4 := ip.To4(); v4 != nil {
		addr.Family = windows.AF_INET
		copy(raw[sockaddrInet4AddrOffset:], v4)
		return nil
	}
	if v6 := ip.To16(); v6 != nil {
		addr.Family = windows.AF_INET6
		copy(raw[sockaddrInet6AddrOffset:], v6)
		return nil
	}
	return fmt.Errorf("invalid ip address %q", ip)
}

// GetDefaultRoute returns the default route for ipVersion (4 or 6) that Windows would use: of
// those present, the lowest by route metric plus interface metric.
func GetDefaultRoute(ipVersion int) (*DefaultRoute, error) {
	family := uint16(windows.AF_INET)
	if ipVersion == 6 {
		family = windows.AF_INET6
	}

	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(family, &table); err != nil {
		return nil, fmt.Errorf("get ipv%d routes: %w", ipVersion, err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)

	var best *windows.MibIpForwardRow2
	var bestMetric uint64
	for i := range rows {
		row := &rows[i]
		if row.DestinationPrefix.PrefixLength != 0 {
			continue
		}
		ifRow := windows.MibIpInterfaceRow{Family: family, InterfaceLuid: row.InterfaceLuid}
		if err := windows.GetIpInterfaceEntry(&ifRow); err != nil || ifRow.Connected == 0 {
			continue
		}
		metric := uint64(row.Metric) + uint64(ifRow.Metric)
		if best == nil || metric < bestMetric {
			best, bestMetric = row, metric
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no ipv%d default route found", ipVersion)
	}

	route := &DefaultRoute{Index: best.InterfaceIndex}
	if gateway := sockaddrIP(&best.NextHop); gateway != nil && !gateway.IsUnspecified() {
		route.Gateway = gateway
	}
	return route, nil
}

// newRoute builds the route to prefix over the interface with the given index, through gateway,
// or on-link if gateway is nil.
func newRoute(prefix *net.IPNet, gateway net.IP, index uint32) (windows.MibIpForwardRow2, error) {
	var row windows.MibIpForwardRow2
	procInitializeIpForwardEntry.Call(uintptr(unsafe.Pointer(&row))) //nolint:errcheck // returns nothing

	row.InterfaceIndex = index
	row.Metric = 0 // leaves the choice to the interface's metric
	if err := setSockaddrIP(&row.DestinationPrefix.Prefix, prefix.IP); err != nil {
		return row, err
	}
	ones, _ := prefix.Mask.Size()
	row.DestinationPrefix.PrefixLength = uint8(ones) //nolint:gosec // at most 128

	if gateway == nil {
		// On-link: an all-zero next hop of the route's own family.
		gateway = net.IPv4zero
		if prefix.IP.To4() == nil {
			gateway = net.IPv6zero
		}
	}
	if err := setSockaddrIP(&row.NextHop, gateway); err != nil {
		return row, err
	}
	return row, nil
}

func createRoute(row *windows.MibIpForwardRow2) error {
	if r, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(row))); r != 0 {
		return windows.Errno(r)
	}
	return nil
}

// deleteRoute treats a route that's already gone, as it is once its adapter is, as deleted.
func deleteRoute(row *windows.MibIpForwardRow2) error {
	r, _, _ := procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(row)))
	err := windows.Errno(r)
	if r == 0 || errors.Is(err, windows.ERROR_NOT_FOUND) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	return err
}

// PinHostRoute keeps ip going through this machine's normal default route of its family after
// RouteAll, so the WebRTC transport never gets routed into the tunnel it's carrying. Close removes
// it. A route that's already there isn't ours to remove, and not an error.
func (d *Device) PinHostRoute(ip net.IP) error {
	ipVersion, bits := 6, net.IPv6len*8
	if ip.To4() != nil {
		ipVersion, bits = 4, net.IPv4len*8
	}
	route, err := GetDefaultRoute(ipVersion)
	if err != nil {
		return err
	}
	prefix := &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
	row, err := newRoute(prefix, route.Gateway, route.Index)
	if err != nil {
		return err
	}
	if err := createRoute(&row); err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
			return nil
		}
		return fmt.Errorf("pin route to %s: %w", ip, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.hostRoutes[ip.String()] = row
	return nil
}

// removeHostRoutes needs d.mu held.
func (d *Device) removeHostRoutes() {
	for key := range d.hostRoutes {
		row := d.hostRoutes[key]
		_ = deleteRoute(&row)
		delete(d.hostRoutes, key)
	}
}

// RouteAll routes all of ipVersion's traffic into the device, by splitting the address space in
// two, the way OpenVPN's def1 and WireGuard do: being more specific, the halves win over the
// default route whatever its metric, without touching it, and they go away with the adapter.
// Blocking IPv6 is this with ipVersion 6, as Read drops what comes in.
func (d *Device) RouteAll(ipVersion int) error {
	halves := []string{"0.0.0.0/1", "128.0.0.0/1"}
	if ipVersion == 6 {
		halves = []string{"::/1", "8000::/1"}
	}
	for _, half := range halves {
		_, prefix, err := net.ParseCIDR(half)
		if err != nil {
			return err
		}
		row, err := newRoute(prefix, nil, d.index)
		if err != nil {
			return err
		}
		if err := createRoute(&row); err != nil {
			return fmt.Errorf("add route %s: %w", half, err)
		}
	}
	return nil
}
