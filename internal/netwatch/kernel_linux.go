//go:build linux

package netwatch

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

// subscribeKernel listens on a NETLINK_ROUTE socket for IPv4 address and
// route changes and link state, calling notify on each message. The
// socket has a one-second receive timeout so the loop can notice ctx
// ending; closing a raw fd from another goroutine would not wake it.
// live(true) once the socket is listening, live(false) when it stops.
func subscribeKernel(ctx context.Context, notify func(), live func(bool)) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	sa := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_LINK,
	}
	if err := unix.Bind(fd, sa); err != nil {
		return
	}
	live(true)
	defer live(false)
	tv := unix.NsecToTimeval(int64(time.Second))
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	buf := make([]byte, 64*1024)
	for ctx.Err() == nil {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
				continue
			}
			return
		}
		if n > 0 {
			notify()
		}
	}
}
