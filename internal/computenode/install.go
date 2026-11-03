package computenode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Label names the service everywhere: launchd's job, systemd's unit.
const Label = "lan.fog.node"

// Service is how the node is kept running on this machine.
type Service struct {
	// Exe is the fog-node binary the service starts; Dir its state.
	Exe string
	Dir string
}

// Path is where the service definition goes.
func (s Service) Path() string {
	home, _ := os.UserHomeDir()
	switch {
	case runtime.GOOS == "darwin":
		// A LaunchAgent, in the owner's session: VideoToolbox and the
		// GPU are there, and the agent never needs root.
		return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
	case os.Geteuid() == 0:
		return "/etc/systemd/system/fog-node.service"
	default:
		return filepath.Join(home, ".config", "systemd", "user", "fog-node.service")
	}
}

// Definition renders the service file.
func (s Service) Definition() string {
	if runtime.GOOS == "darwin" {
		log := filepath.Join(s.Dir, "fog-node.log")
		return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(s.Exe) + `</string>
		<string>run</string>
		<string>--dir</string>
		<string>` + xmlEscape(s.Dir) + `</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ProcessType</key><string>Background</string>
	<key>StandardOutPath</key><string>` + xmlEscape(log) + `</string>
	<key>StandardErrorPath</key><string>` + xmlEscape(log) + `</string>
</dict>
</plist>
`
	}
	target := "multi-user.target"
	if os.Geteuid() != 0 {
		target = "default.target"
	}
	return fmt.Sprintf(`[Unit]
Description=fog-node - lends this machine's hardware to fog
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run --dir %s
Restart=always
RestartSec=3

[Install]
WantedBy=%s
`, strconv.Quote(s.Exe), strconv.Quote(s.Dir), target)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Install writes the service and starts it, replacing one already there.
func (s Service) Install() error {
	p := s.Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(s.Definition()), 0o644); err != nil {
		return err
	}
	switch {
	case runtime.GOOS == "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = exec.Command("launchctl", "bootout", domain+"/"+Label).Run()
		if out, err := exec.Command("launchctl", "bootstrap", domain, p).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
		}
	default:
		args := []string{}
		if os.Geteuid() != 0 {
			args = append(args, "--user")
		}
		for _, cmd := range [][]string{{"daemon-reload"}, {"enable", "--now", "fog-node.service"}, {"restart", "fog-node.service"}} {
			if out, err := exec.Command("systemctl", append(args, cmd...)...).CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl %s: %v: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

// Uninstall stops the service and removes it.
func (s Service) Uninstall() error {
	switch {
	case runtime.GOOS == "darwin":
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+Label).Run()
	default:
		args := []string{}
		if os.Geteuid() != 0 {
			args = append(args, "--user")
		}
		_ = exec.Command("systemctl", append(args, "disable", "--now", "fog-node.service")...).Run()
	}
	err := os.Remove(s.Path())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
