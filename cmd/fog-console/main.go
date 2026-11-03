// fog-console keeps the machine's own display useful without taking it over.
// tty1 is an ordinary Debian login: agetty prints /etc/issue and a prompt,
// and logging in with the admin account gives a normal bash shell. This
// program owns only that banner — the server's name, where to reach it, and
// whether Fog is healthy — rewriting /etc/issue whenever the facts change so
// a glance at an attached monitor still answers "where is my server and is
// it okay" before anyone types a thing.
//
// It deliberately depends on almost nothing: a static binary reading the
// hostname, the interface table, /etc/passwd, and fogd's unauthenticated
// status endpoint. When fogd is down it says so, plainly, with the first
// thing to try from the shell that is now one login away.
//
// Usage:
//
//	fog-console          maintain /etc/issue (the systemd service)
//	fog-console --once   print the panel once, for login greetings
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
)

const (
	fogdStatus = "http://localhost:8080/api/setup/status"
	issuePath  = "/etc/issue"
	// Writing this file is exactly what `agetty --reload` does: every idle
	// agetty watches /run for it (inotify) and redraws its issue and prompt.
	// A logged-in shell is untouched — agetty is gone by then.
	reloadPath = "/run/agetty.reload"
	// Home + clear: the banner owns the screen above the login prompt.
	clearScreen = "\x1b[H\x1b[2J"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--once" || os.Args[1] == "-1") {
		fmt.Print(render(false))
		return
	}

	var last string
	for {
		frame := clearScreen + render(true)
		// Only on change: a quiet server stops touching the disk, and an
		// operator reading the screen isn't interrupted by a redraw that
		// says the same thing.
		if frame != last {
			if err := writeIssue(frame); err != nil {
				fmt.Fprintln(os.Stderr, "fog-console: writing "+issuePath+": "+err.Error())
			} else {
				last = frame
				reloadGettys()
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func render(forLogin bool) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "fog"
	}
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\r\n") }

	line("")
	line("   ░░░  fog")
	line("   ─────────────────────────────────────────────")
	line("")

	up, version := fogdUp()
	if up {
		line("   ●  Fog is running" + versionSuffix(version))
	} else {
		line("   ○  Fog is not answering")
	}
	line("")
	line("   Manage this server from any browser on your network:")
	line("")
	line("      http://" + host + ".local:8080")
	for _, ip := range lanIPs() {
		line("      http://" + ip + ":8080")
	}
	line("")
	if !up {
		// The recourse is a shell, which is one login away below (or already
		// open, when this is a login greeting).
		lead := "   Try: "
		if forLogin {
			lead = "   Log in below, then: "
		}
		line(lead + "sudo systemctl status fogd")
		line(strings.Repeat(" ", len(lead)) + "sudo journalctl -u fogd -b")
		line("")
	}
	if forLogin {
		// The prompt below is a real Debian login, so say whose it is —
		// a restore run without an admin password leaves nobody to name.
		if who := consoleAccounts(); len(who) > 0 {
			line("   Log in as " + strings.Join(who, " or ") + " for a shell on this server.")
		} else {
			line("   No console account exists on this server; manage it from a browser.")
		}
		line("")
	}
	return b.String()
}

func versionSuffix(v string) string {
	if v == "" {
		return ""
	}
	return "  ·  " + v
}

// writeIssue replaces the banner atomically: agetty may be reading it, and
// half a file is worse than a stale one.
func writeIssue(frame string) error {
	return fsx.WriteFileAtomic(issuePath, []byte(frame), 0o644)
}

func reloadGettys() {
	if err := os.WriteFile(reloadPath, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fog-console: signalling agetty: "+err.Error())
	}
}

// consoleAccounts lists the human accounts that can actually log in here:
// the installer's admin (and any account added since), never the system
// users or the locked root.
func consoleAccounts() []string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseConsoleAccounts(f)
}

func parseConsoleAccounts(r io.Reader) []string {
	var out []string
	scan := bufio.NewScanner(r)
	for scan.Scan() {
		parts := strings.Split(scan.Text(), ":")
		if len(parts) < 7 {
			continue
		}
		uid, err := strconv.Atoi(parts[2])
		if err != nil || uid < 1000 || uid >= 65534 {
			continue
		}
		switch shell := parts[6]; shell {
		case "", "/usr/sbin/nologin", "/sbin/nologin", "/bin/false", "/usr/bin/false":
			continue
		}
		out = append(out, parts[0])
	}
	sort.Strings(out)
	return out
}

func fogdUp() (bool, string) {
	client := http.Client{Timeout: 2 * time.Second}
	res, err := client.Get(fogdStatus)
	if err != nil {
		return false, ""
	}
	// Closed on every path: this polls every five seconds for the life of
	// the service, and a body left open on a non-200 answer would leak one
	// connection per iteration for as long as fogd answers unhappily.
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false, ""
	}
	var body struct {
		Version string `json:"version"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	return true, body.Version
}

func lanIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		// Container bridges aren't how the operator reaches the box.
		if strings.HasPrefix(iface.Name, "docker") || strings.HasPrefix(iface.Name, "br-") || strings.HasPrefix(iface.Name, "veth") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	sort.Strings(out)
	return out
}
