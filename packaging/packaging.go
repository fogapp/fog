// Package packaging carries the files that install fogd on a host, so the
// binary can write them itself: the systemd unit, and fogd-rollback, the
// guard that puts the previous fogd back when a self-update never comes up
// healthy (internal/selfupdate/rollback.go), and fogd.socket, which holds
// fogd's port across restarts. install.sh fetches the same
// files from the release; release.sh publishes these exact bytes.
package packaging

import _ "embed"

//go:embed fogd.service
var Unit []byte

//go:embed fogd-rollback
var Rollback []byte

//go:embed fogd.socket
var Socket []byte
