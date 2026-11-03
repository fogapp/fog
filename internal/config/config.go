// Package config holds fogd's process-level configuration: filesystem paths,
// listen address, and build identity. Everything else — all operator-facing
// configuration — lives in the store and is edited through the UI.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Version is stamped at build time via -ldflags, as
// "-X …/config.Version=fogd-version=vX.Y.Z". The prefix is a marker: the
// stamped string sits in the binary's data as written, so install.sh can
// read the version of an installed fogd from the file, without running it
// (a fogd from before --version would start a second daemon). It is
// removed here, before anything reads Version.
var Version = "dev"

// VersionStamp is the marker before a stamped version.
const VersionStamp = stampName + "="

// stampName is compared without its "=", so the binary never holds the
// bare marker "fogd-version=" anywhere but in the stamp itself.
const stampName = "fogd-version"

func init() { Version = unstamp(Version) }

func unstamp(v string) string {
	if name, rest, ok := strings.Cut(v, "="); ok && name == stampName {
		return rest
	}
	return v
}

type Config struct {
	// DataDir is Fog's state root (default /var/lib/fog). Holds fog.db,
	// master.key, and the materialized apps/, caddy/, data/, builds/,
	// backups/ trees as later phases land.
	DataDir string

	// ListenAddr for the HTTP server. Phase 0 serves plain HTTP on the LAN;
	// Caddy takes over 80/443 in Phase 1.
	ListenAddr string

	// Dev relaxes cookie security for http://localhost development.
	Dev bool
}

func FromEnv() Config {
	c := Config{
		DataDir:    "/var/lib/fog",
		ListenAddr: ":8080",
		Dev:        os.Getenv("FOG_DEV") == "1",
	}
	if v := os.Getenv("FOG_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("FOG_LISTEN"); v != "" {
		c.ListenAddr = v
	}
	return c
}

func (c Config) DBPath() string { return filepath.Join(c.DataDir, "fog.db") }
