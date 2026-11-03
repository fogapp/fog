// Package computenode is fog-node: the agent on a machine that lends fog
// its hardware (docs/compute-contract.md). It pairs with fog once,
// reports what the machine can do, and runs the work fog hands it within
// the machine's budget.
package computenode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

// FindFFmpeg answers the ffmpeg to use: the configured one, else the first
// on PATH or where Homebrew puts it (a launchd job's PATH is short).
func FindFFmpeg(configured string) string {
	if configured != "" {
		return configured
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// renderNode is where a Linux machine's GPU takes VA-API work.
const renderNode = "/dev/dri/renderD128"

// Default budgets per family, before the operator sets one: what the spike
// measured an M1's media engine to keep up with at live speed, rounded
// down, and Stream's own default for an Intel iGPU
// (docs/compute-contract.md).
var defaultSlots = map[string]compute.Slots{
	"videotoolbox": {Interactive: 4, Batch: 2},
	"vaapi":        {Interactive: 3, Batch: 1},
}

// Probe asks the machine what it can do.
func Probe(ctx context.Context, name, ffmpeg, version string) compute.Inventory {
	inv := compute.Inventory{Name: name, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: version,
		CPUs: runtime.NumCPU(), MemoryBytes: memoryBytes()}
	if inv.Name == "" {
		inv.Name = DefaultName()
	}
	if ffmpeg != "" {
		inv.Video = probeVideo(ctx, ffmpeg)
	}
	return inv
}

// DefaultName is the machine's own name: a Mac's computer name, else its
// hostname without the domain.
func DefaultName() string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil {
			if n := strings.TrimSpace(string(out)); n != "" {
				return n
			}
		}
	}
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	if h == "" {
		return "machine"
	}
	return h
}

func probeVideo(ctx context.Context, ffmpeg string) *compute.VideoCaps {
	version := firstLine(capture(ctx, ffmpeg, "-hide_banner", "-version"))
	if version == "" {
		return nil
	}
	version, _, _ = strings.Cut(strings.TrimPrefix(version, "ffmpeg version "), " ")
	v := &compute.VideoCaps{FFmpeg: version, Family: "software"}
	v.Encoders = videoEncoders(capture(ctx, ffmpeg, "-hide_banner", "-encoders"))
	v.Filters = filterNames(capture(ctx, ffmpeg, "-hide_banner", "-filters"))
	switch {
	case slices.Contains(v.Encoders, "h264_videotoolbox") && encodes(ctx, ffmpeg, nil, []string{"-c:v", "h264_videotoolbox"}):
		v.Family = "videotoolbox"
	case slices.Contains(v.Encoders, "h264_vaapi") && exists(renderNode) &&
		encodes(ctx, ffmpeg, []string{"-init_hw_device", "vaapi=va:" + renderNode, "-filter_hw_device", "va"},
			[]string{"-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi"}):
		v.Family = "vaapi"
	}
	v.Slots = defaultSlots[v.Family]
	return v
}

// encodes reports whether one real frame goes through an encoder: an
// encoder listed but without its device or driver must not be chosen.
func encodes(ctx context.Context, ffmpeg string, before, codec []string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	argv := append([]string{"-hide_banner", "-v", "error"}, before...)
	argv = append(argv, "-f", "lavfi", "-i", "testsrc2=size=320x240:duration=0.1")
	argv = append(argv, codec...)
	argv = append(argv, "-frames:v", "1", "-f", "null", "-")
	return exec.CommandContext(ctx, ffmpeg, argv...).Run() == nil
}

var encoderLine = regexp.MustCompile(`(?m)^\s*V[.A-Z]{5}\s+(\S+)`)

// videoEncoders keeps the H.264/HEVC ones apps choose between.
func videoEncoders(listing string) []string {
	var out []string
	for _, m := range encoderLine.FindAllStringSubmatch(listing, -1) {
		name := m[1]
		if strings.HasPrefix(name, "h264") || strings.HasPrefix(name, "hevc") || name == "libx264" || name == "libx265" {
			out = append(out, name)
		}
	}
	return out
}

// filterLine is one row of `ffmpeg -filters`: flags, then the name.
var filterLine = regexp.MustCompile(`(?m)^[ \t]*\S+[ \t]+(\w+)[ \t]`)

func filterNames(listing string) []string {
	var out []string
	for _, m := range filterLine.FindAllStringSubmatch(listing, -1) {
		out = append(out, m[1])
	}
	return out
}

func capture(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// memoryBytes is the machine's RAM, where the platform says.
func memoryBytes() uint64 {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			n, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			return n
		}
	case "linux":
		raw, err := os.ReadFile(filepath.Join("/proc", "meminfo"))
		if err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
					kb, _ := strconv.ParseUint(f[1], 10, 64)
					return kb * 1024
				}
			}
		}
	}
	return 0
}

// memoryPressed reports a Mac short of memory (the kernel's pressure level
// past normal): its models want the memory, so batch work waits.
func memoryPressed() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output()
	if err != nil {
		return false
	}
	level, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return level > 1
}
