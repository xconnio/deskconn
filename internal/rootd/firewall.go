//go:build linux

package rootd

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// The exit node's NAT and forwarding rules go in with whichever firewall tool the distro
// has: iptables (legacy, or the iptables-nft shim most distros ship now) if it works,
// since its rules land in the same chains as ufw's and Docker's and so can get ahead of
// their DROPs, or else nft directly (e.g. Debian or Fedora installs with only nftables).
type firewallBackend int

const (
	backendIPTables firewallBackend = iota
	backendNFT
)

// nftTable holds the nft backend's rules, so that removing them is deleting the table.
const nftTable = "deskconn"

// firewall installs the exit node's rules, remembering what it needs to take them out again.
type firewall struct {
	detectOnce sync.Once
	backend    firewallBackend
	detectErr  error

	mu      sync.Mutex
	trusted map[string]bool // interfaces trustInFirewalld put in firewalld's trusted zone
}

// detect picks the backend once, so rules are always removed with the tool that added them.
func (f *firewall) detect() (firewallBackend, error) {
	f.detectOnce.Do(func() { f.backend, f.detectErr = detectFirewallBackend() })
	return f.backend, f.detectErr
}

func detectFirewallBackend() (firewallBackend, error) {
	// Listing proves iptables can actually reach the kernel: iptables-legacy fails on a
	// kernel built without the legacy xtables modules.
	if _, err := exec.LookPath("iptables"); err == nil {
		if err := runNetCmd("iptables", "-w", "-t", "nat", "-S", "POSTROUTING"); err == nil {
			return backendIPTables, nil
		}
	}
	if _, err := exec.LookPath("nft"); err == nil {
		return backendNFT, nil
	}
	return 0, fmt.Errorf("serving as a vpn exit node needs iptables or nft, and neither works here")
}

// AddMasquerade adds a MASQUERADE rule so traffic from subnet leaving via oif
// gets the host's own address.
func (f *firewall) AddMasquerade(subnet, oif string) error {
	backend, err := f.detect()
	if err != nil {
		return err
	}
	if backend == backendNFT {
		// "table ...; delete table ..." first clears whatever a previous run left behind
		// (the add makes the delete safe when there's nothing), all in one transaction.
		script := fmt.Sprintf(`table ip %[1]s
delete table ip %[1]s
table ip %[1]s {
	chain postrouting {
		type nat hook postrouting priority 100; policy accept;
		ip saddr %[2]s oifname "%[3]s" masquerade
	}
}
`, nftTable, subnet, oif)
		return runNetCmdInput(script, "nft", "-f", "-")
	}
	return runNetCmd("iptables", "-w", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-o", oif, "-j", "MASQUERADE")
}

// DelMasquerade removes a rule added by AddMasquerade.
func (f *firewall) DelMasquerade(subnet, oif string) error {
	backend, err := f.detect()
	if err != nil {
		return err
	}
	if backend == backendNFT {
		return runNetCmd("nft", "delete", "table", "ip", nftTable)
	}
	return runNetCmd("iptables", "-w", "-t", "nat", "-D", "POSTROUTING", "-s", subnet, "-o", oif, "-j", "MASQUERADE")
}

// AddForwardAccept allows forwarding from inIface to outIface. iptables rules are
// inserted at the head of FORWARD (-I, not -A) so they take effect ahead of
// any earlier DROP/REJECT rule a distro or another tool (e.g. Docker or ufw) may
// already have installed further down the chain.
//
// nft has nothing to add: an accept only ends its own chain, so it can't
// overrule another table's drop, and with no drop of ours forwarding is
// already allowed. firewalld (Fedora, RHEL, openSUSE) rejects forwarding
// from interfaces outside its zones in its own table whichever backend
// is in use, so inIface also goes in its trusted zone, until the next
// firewalld reload.
func (f *firewall) AddForwardAccept(inIface, outIface string) error {
	backend, err := f.detect()
	if err != nil {
		return err
	}
	if backend == backendIPTables {
		if err := runNetCmd("iptables", "-w", "-I", "FORWARD", "-i", inIface, "-o", outIface, "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	if err := f.trustInFirewalld(inIface); err != nil {
		if backend == backendIPTables {
			_ = runNetCmd("iptables", "-w", "-D", "FORWARD", "-i", inIface, "-o", outIface, "-j", "ACCEPT")
		}
		return err
	}
	return nil
}

// DelForwardAccept removes a rule added by AddForwardAccept.
func (f *firewall) DelForwardAccept(inIface, outIface string) error {
	backend, err := f.detect()
	if err != nil {
		return err
	}
	f.untrustInFirewalld(inIface)
	if backend == backendIPTables {
		return runNetCmd("iptables", "-w", "-D", "FORWARD", "-i", inIface, "-o", outIface, "-j", "ACCEPT")
	}
	return nil
}

// AddForwardEstablished allows return traffic for connections the tunnel
// initiated. See AddForwardAccept for why nft needs nothing.
func (f *firewall) AddForwardEstablished(inIface, outIface string) error {
	backend, err := f.detect()
	if err != nil || backend != backendIPTables {
		return err
	}
	return runNetCmd("iptables", "-w", "-I", "FORWARD", "-i", inIface, "-o", outIface,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
}

// DelForwardEstablished removes a rule added by AddForwardEstablished.
func (f *firewall) DelForwardEstablished(inIface, outIface string) error {
	backend, err := f.detect()
	if err != nil || backend != backendIPTables {
		return err
	}
	return runNetCmd("iptables", "-w", "-D", "FORWARD", "-i", inIface, "-o", outIface,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
}

// firewalldRunning reports whether firewalld is installed and running.
func firewalldRunning() bool {
	if _, err := exec.LookPath("firewall-cmd"); err != nil {
		return false
	}
	return exec.Command("firewall-cmd", "--state").Run() == nil
}

// trustInFirewalld puts iface in firewalld's trusted zone (runtime only), if firewalld is
// running. An iface the operator already gave a zone of their own is left in it.
func (f *firewall) trustInFirewalld(iface string) error {
	if !firewalldRunning() {
		return nil
	}

	out, err := exec.Command("firewall-cmd", "--get-zone-of-interface="+iface).Output() //nolint:gosec
	if zone := strings.TrimSpace(string(out)); err == nil && zone != "" {
		log.Debugf("deskconn-rootd: %s is already in firewalld zone %s, leaving it there", iface, zone)
		return nil
	}

	if err := runNetCmd("firewall-cmd", "--zone=trusted", "--add-interface="+iface); err != nil {
		return err
	}
	f.mu.Lock()
	if f.trusted == nil {
		f.trusted = map[string]bool{}
	}
	f.trusted[iface] = true
	f.mu.Unlock()
	return nil
}

// untrustInFirewalld undoes trustInFirewalld.
func (f *firewall) untrustInFirewalld(iface string) {
	f.mu.Lock()
	trusted := f.trusted[iface]
	delete(f.trusted, iface)
	f.mu.Unlock()
	if !trusted {
		return
	}
	if err := runNetCmd("firewall-cmd", "--zone=trusted", "--remove-interface="+iface); err != nil {
		log.Debugf("deskconn-rootd: failed to take %s back out of firewalld's trusted zone: %v", iface, err)
	}
}
