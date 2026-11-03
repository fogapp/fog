//go:build unix

package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// listenFDsStart is the first descriptor systemd passes (sd_listen_fds).
const listenFDsStart = 3

// Listener returns the listening socket systemd passed this process
// (socket activation: packaging/fogd.socket), or nil when there is none —
// a dev run, a unit without the socket — and the caller binds its own. The
// socket outlives the process, which is the point: the port is never free
// between one fogd and the next. The LISTEN_* variables are cleared so a
// child never mistakes them for its own.
func Listener() (net.Listener, error) {
	defer func() {
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
	}()
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n < 1 {
		return nil, nil
	}
	if n != 1 {
		return nil, fmt.Errorf("systemd passed %d sockets; fogd takes one", n)
	}
	syscall.CloseOnExec(listenFDsStart)
	f := os.NewFile(uintptr(listenFDsStart), "fogd.socket")
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("the socket systemd passed: %w", err)
	}
	return ln, nil
}
