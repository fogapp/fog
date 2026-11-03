package setup

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gitea.lan/matthew/fog/internal/execx"
)

// bootDiskName resolves which whole disk the live medium is on, so the
// installer can never offer to erase the stick it booted from. The file is
// written at boot by fog-bootdisk (live overlay); "" means the recording
// failed, in which case the FOG-BOOT label check in parseLsblk still
// identifies the stick and Validate refuses removable targets outright.
func bootDiskName() string {
	raw, err := os.ReadFile("/run/fog-installer/boot-disk")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// markerMount is where the ESP is briefly mounted to read the marker. Its
// own directory, never a path anything else in the installer uses.
const markerMount = "/run/fog-installer/esp-probe"

var markerCache struct {
	sync.Mutex
	found map[string]*InstallMarker
}

// readInstallMarker mounts a disk's ESP read-only and reads the marker a
// finished install left there. Read-only and immediately unmounted: this
// runs on every inventory refresh, against a disk the operator may be about
// to keep, and inspecting it must not be able to change it.
//
// A found marker is remembered for the rest of this boot — the wizard
// refreshes the inventory on a timer, and the answer can only change by way
// of an install this installer itself runs. A miss is not cached: a disk
// mid-format answers "no marker" for a few seconds, and that must not
// become the session's verdict.
func readInstallMarker(espDev string) *InstallMarker {
	markerCache.Lock()
	defer markerCache.Unlock()
	if m, ok := markerCache.found[espDev]; ok {
		return m
	}
	if err := os.MkdirAll(markerMount, 0o755); err != nil {
		return nil
	}
	// Every step is deadline-bounded (runProbe): this runs on each inventory
	// refresh while holding markerCache's lock, and a disk hanging in D-state
	// must cost one timeout — not freeze every inventory request forever. The
	// read goes through cat for the same reason; os.ReadFile against a hung
	// filesystem is uninterruptible.
	if _, err := runProbe("mount", "-o", "ro", espDev, markerMount); err != nil {
		return nil
	}
	defer runProbe("umount", markerMount)
	raw, err := runProbe("cat", filepath.Join(markerMount, InstallMarkerName))
	if err != nil {
		return nil
	}
	m := parseInstallMarker(raw)
	if m != nil {
		if markerCache.found == nil {
			markerCache.found = map[string]*InstallMarker{}
		}
		markerCache.found[espDev] = m
	}
	return m
}

// liveNetwork reports the default-route interface and its address — the
// wizard's whole question is "can the installed server be reached, and at
// what IP".
func liveNetwork() Network {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	raw, err := execx.Command(ctx, "ip", "-json", "route", "show", "default").Output()
	if err != nil {
		return Network{}
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	if json.Unmarshal(raw, &routes) != nil || len(routes) == 0 {
		return Network{}
	}
	dev := routes[0].Dev
	iface, err := net.InterfaceByName(dev)
	if err != nil {
		return Network{Online: true, Interface: dev}
	}
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return Network{Online: true, Interface: dev, IP: ipn.IP.String()}
		}
	}
	return Network{Online: true, Interface: dev}
}

func dmiProductName() string {
	raw, err := os.ReadFile("/sys/class/dmi/id/product_name")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
