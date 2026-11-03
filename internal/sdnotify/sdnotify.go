// Package sdnotify speaks just enough of systemd's notification protocol for
// Type=notify and WatchdogSec: READY, WATCHDOG, and STOPPING. Every call is
// a silent no-op when NOTIFY_SOCKET is unset (dev runs, tests, non-systemd
// hosts), so callers never need to care where they're running.
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"time"
)

// Notify sends one state line to the supervising systemd, if any.
func Notify(state string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	conn.Write([]byte(state))
}

// WatchdogInterval returns the heartbeat deadline systemd armed for this
// process. The caller should send Notify("WATCHDOG=1") at half this interval
// while — and only while — it considers itself healthy: a hung process that
// stops heartbeating is exactly what WatchdogSec exists to restart.
func WatchdogInterval() (time.Duration, bool) {
	usec := os.Getenv("WATCHDOG_USEC")
	if usec == "" {
		return 0, false
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0, false
	}
	n, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return time.Duration(n) * time.Microsecond, true
}
