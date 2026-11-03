//go:build !unix

package sdnotify

import "net"

// Listener: no socket activation off unix; the caller binds its own.
func Listener() (net.Listener, error) { return nil, nil }
