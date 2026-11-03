package wan

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/execx"
)

// Gateway returns the default IPv4 gateway — the router NAT-PMP and UPnP
// talk to, and the address the operator opens in a browser when the
// automatic route fails and a port has to be forwarded by hand.
func Gateway() (net.IP, error) {
	if runtime.GOOS == "darwin" {
		return gatewayDarwin()
	}
	return gatewayProcRoute("/proc/net/route")
}

// gatewayProcRoute reads the kernel routing table: the default route is
// the entry with destination 0.0.0.0 and the gateway flag set. Addresses
// are hex, little-endian.
func gatewayProcRoute(path string) (net.IP, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		ip := net.IPv4(raw[3], raw[2], raw[1], raw[0])
		if ip.IsUnspecified() {
			continue
		}
		return ip, nil
	}
	return nil, errors.New("no default route")
}

func gatewayDarwin() (net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := execx.Command(ctx, "route", "-n", "get", "default").Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "gateway:"); ok {
			if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errors.New("no default route")
}
