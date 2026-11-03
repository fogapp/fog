package setup

import (
	"context"
	"net"
	"regexp"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/execx"
)

// Both run through execx so a cancelled context kills the whole process
// group — unsquashfs, cp -a, and chrooted tools all fork children that
// plain CommandContext would leave running against a disk mid-write.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	out, err := execx.Command(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func runCmdStdin(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := execx.Command(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var squashSizeRe = regexp.MustCompile(`Filesystem size (\d+)`)

// squashUncompressedBytes asks unsquashfs for the archive's declared
// uncompressed size ("Filesystem size N bytes"), the denominator for honest
// extraction progress. 0 when it can't be known.
func squashUncompressedBytes(path string) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := execx.Command(ctx, "unsquashfs", "-s", path).Output()
	if err != nil {
		return 0
	}
	// -s reports "Filesystem size N bytes" among its stats.
	m := squashSizeRe.FindSubmatch(out)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(string(m[1]), 10, 64)
	return n
}

func currentIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				return ipn.IP.String()
			}
		}
	}
	return ""
}
