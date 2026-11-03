package setup

import (
	"encoding/json"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
)

// InstallMarkerName is the marker's name on the installed machine's ESP —
// which the running system mounts at /boot, so it is also readable there.
const InstallMarkerName = "fog-install.json"

// InstallMarker records that an install ran to completion on this disk. It
// is written as the last act of a successful install and read back by a
// later boot of the installer — the state you land in when the USB stick
// was left plugged in and the machine restarted into it.
//
// Only a finished install writes it, so its presence is the one honest
// signal for "this machine is already a Fog server": the four fog labels
// alone also survive an install that died halfway through formatting.
type InstallMarker struct {
	ServerName  string `json:"serverName"`
	Mode        string `json:"mode"` // fresh | restore
	Version     string `json:"version"`
	InstalledAt string `json:"installedAt"` // RFC3339, UTC
}

func writeInstallMarker(path string, m InstallMarker) error {
	if m.InstalledAt == "" {
		m.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Atomic: the marker is the last word of an install, and a power cut
	// mid-write must leave either no marker or a whole one.
	return fsx.WriteFileAtomic(path, append(raw, '\n'), 0o644)
}

// parseInstallMarker reads a marker, rejecting one that names no install
// date — a truncated or foreign file must not be read as a finished
// install.
func parseInstallMarker(raw []byte) *InstallMarker {
	var m InstallMarker
	if json.Unmarshal(raw, &m) != nil || m.InstalledAt == "" {
		return nil
	}
	return &m
}
