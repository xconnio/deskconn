//go:build linux

package rootd

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"

	"github.com/xconnio/deskconn/internal/common"
)

const (
	resolvConfPath = "/etc/resolv.conf"

	// resolvConfMarker starts the resolv.conf SetLinkDNS writes, so RevertLinkDNS can tell
	// whether it's still the one in place.
	resolvConfMarker = "# Written by deskconn-rootd while a VPN tunnel is up"
)

// resolvConfBackup is what /etc/resolv.conf was before SetLinkDNS replaced it: a symlink's
// target (e.g. NetworkManager's or resolvconf's file), or a regular file's contents.
type resolvConfBackup struct {
	link    string
	content []byte
	mode    os.FileMode
}

// linkDNS points this machine's lookups at a tunnel's resolvers and back.
type linkDNS struct {
	mu    sync.Mutex
	saved *resolvConfBackup // non-nil while SetLinkDNS's resolv.conf is in place
}

// SetLinkDNS sends every lookup to servers over iface. With systemd-resolved
// answering this machine's lookups that's a per-link setting: "~." makes iface
// the routing domain for all names, so no other link's resolvers are asked
// (except for more specific domains, e.g. a LAN search domain), and
// systemd-resolved drops it by itself once iface is gone. Anywhere else
// (resolvconf, NetworkManager or a static file) /etc/resolv.conf is swapped
// for one naming only servers, until RevertLinkDNS puts it back. Every server
// must be an IP address.
func (d *linkDNS) SetLinkDNS(iface string, servers []string) error {
	if len(servers) == 0 {
		return fmt.Errorf("no dns servers given")
	}
	for _, s := range servers {
		if net.ParseIP(s) == nil {
			return fmt.Errorf("invalid dns server %q", s)
		}
	}

	if !resolvedHandlesLookups() {
		return d.replaceResolvConf(servers)
	}

	if err := runNetCmd("resolvectl", append([]string{"dns", iface}, servers...)...); err != nil {
		return err
	}
	if err := runNetCmd("resolvectl", "domain", iface, "~."); err != nil {
		_ = d.RevertLinkDNS(iface)
		return err
	}
	if err := runNetCmd("resolvectl", "default-route", iface, "yes"); err != nil {
		_ = d.RevertLinkDNS(iface)
		return err
	}
	// Answers cached from before would otherwise keep pointing lookups at servers picked for
	// the old network.
	_ = runNetCmd("resolvectl", "flush-caches")
	return nil
}

// RevertLinkDNS undoes SetLinkDNS.
func (d *linkDNS) RevertLinkDNS(iface string) error {
	d.mu.Lock()
	replaced := d.saved != nil
	d.mu.Unlock()
	if replaced {
		return d.restoreResolvConf()
	}

	err := runNetCmd("resolvectl", "revert", iface)
	_ = runNetCmd("resolvectl", "flush-caches")
	return err
}

// resolvedHandlesLookups reports whether /etc/resolv.conf sends lookups to
// systemd-resolved's stub, i.e. whether per-link settings made with resolvectl
// actually take effect. Not the case on distros without systemd-resolved, nor
// where it runs but resolv.conf is managed some other way.
func resolvedHandlesLookups() bool {
	if _, err := exec.LookPath("resolvectl"); err != nil {
		return false
	}
	data, err := os.ReadFile(resolvConfPath)
	if err != nil {
		return false
	}
	for _, server := range common.ParseResolvConfNameservers(data) {
		if server == "127.0.0.53" || server == "127.0.0.54" {
			return true
		}
	}
	return false
}

func (d *linkDNS) replaceResolvConf(servers []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.saved != nil {
		return fmt.Errorf("%s is already replaced", resolvConfPath)
	}

	backup := &resolvConfBackup{}
	info, err := os.Lstat(resolvConfPath)
	switch {
	case os.IsNotExist(err):
		// Nothing to keep; restoring just removes ours.
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		if backup.link, err = os.Readlink(resolvConfPath); err != nil {
			return err
		}
	default:
		if backup.content, err = os.ReadFile(resolvConfPath); err != nil {
			return err
		}
		backup.mode = info.Mode().Perm()
	}

	var buf bytes.Buffer
	buf.WriteString(resolvConfMarker + "; the original comes back once it closes.\n")
	for _, s := range servers {
		fmt.Fprintf(&buf, "nameserver %s\n", s)
	}
	// Written alongside and renamed over, so lookups never see a half-written file and a
	// symlink is replaced rather than written through (which would change its target instead).
	if err := writeFileAtomic(resolvConfPath, func(tmp string) error {
		return os.WriteFile(tmp, buf.Bytes(), 0o644) //nolint:gosec
	}); err != nil {
		return err
	}
	d.saved = backup
	return nil
}

func (d *linkDNS) restoreResolvConf() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	backup := d.saved
	if backup == nil {
		return nil
	}
	d.saved = nil

	// Something else (e.g. NetworkManager on reconnecting) may have written a fresh one while
	// the tunnel was up; that one is newer than the backup, so leave it.
	if current, err := os.ReadFile(resolvConfPath); err == nil && !bytes.HasPrefix(current, []byte(resolvConfMarker)) {
		return nil
	}

	switch {
	case backup.link != "":
		return writeFileAtomic(resolvConfPath, func(tmp string) error { return os.Symlink(backup.link, tmp) })
	case backup.content != nil:
		return writeFileAtomic(resolvConfPath, func(tmp string) error {
			return os.WriteFile(tmp, backup.content, backup.mode)
		})
	default:
		err := os.Remove(resolvConfPath)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
}

// writeFileAtomic has create make a new file at a temporary path next to path, then renames
// it over path.
func writeFileAtomic(path string, create func(tmp string) error) error {
	tmp := path + ".deskconn"
	_ = os.Remove(tmp)
	if err := create(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
