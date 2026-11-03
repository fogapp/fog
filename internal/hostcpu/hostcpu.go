// Package hostcpu answers one question about the machine fogd runs on: which
// x86-64 microarchitecture level its processor exposes. Prebuilt binaries
// increasingly assume x86-64-v2 (SSE4.2, POPCNT — every physical x86 CPU
// since 2009), and a VM whose hypervisor presents the generic "kvm64" model
// hides those instructions from an otherwise capable host. The failure is a
// crash loop with a stack trace; the fix is a setting on another machine.
// Knowing the level up front lets the app store say so before the install.
package hostcpu

import (
	"runtime"
	"strings"
	"sync"
)

// Info describes the processor as the guest kernel sees it.
type Info struct {
	Arch string `json:"arch"` // runtime.GOARCH
	// Model is the processor's marketing name ("Common KVM processor",
	// "Intel(R) Core(TM) i5-8500"), used to name the culprit in a message.
	Model string `json:"model,omitempty"`
	// Level is the x86-64 microarchitecture level (1–4) on amd64; 0 when the
	// flags couldn't be read or the architecture has no such levels.
	Level int `json:"level,omitempty"`
	// Known reports whether the flags were actually read. On a platform
	// without /proc/cpuinfo nothing is claimed either way.
	Known bool `json:"known"`
	// Virtual is set when the CPU carries the hypervisor flag — the case
	// where a missing level is a VM setting, not a hardware limit.
	Virtual bool `json:"virtual"`
}

// Levels are cumulative: a v3 processor has every v2 flag as well. Names are
// as Linux prints them in /proc/cpuinfo (abm carries LZCNT; xsave stands in
// for OSXSAVE, which the kernel folds into it).
var levelFlags = [][]string{
	{"cx16", "lahf_lm", "popcnt", "sse4_1", "sse4_2", "ssse3"},              // v2
	{"avx", "avx2", "bmi1", "bmi2", "f16c", "fma", "abm", "movbe", "xsave"}, // v3
	{"avx512f", "avx512bw", "avx512cd", "avx512dq", "avx512vl"},             // v4
}

// Describe names what a level adds, for the person reading the message.
func Describe(level int) string {
	switch level {
	case 2:
		return "SSE4.2, POPCNT"
	case 3:
		return "AVX2"
	case 4:
		return "AVX-512"
	}
	return ""
}

var (
	once sync.Once
	info Info
)

// Detect reads the processor once per process — the flags do not change
// while fogd runs.
func Detect() Info {
	once.Do(func() { info = detect() })
	return info
}

// Parse derives Info from the text of /proc/cpuinfo. Exported for tests and
// for the one caller that already has the file.
func Parse(cpuinfo string, arch string) Info {
	in := Info{Arch: arch}
	var flags map[string]bool
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "model name":
			if in.Model == "" {
				in.Model = val
			}
		case "flags":
			if flags == nil {
				flags = map[string]bool{}
				for _, f := range strings.Fields(val) {
					flags[f] = true
				}
			}
		}
	}
	if flags == nil {
		return in
	}
	in.Known = true
	in.Virtual = flags["hypervisor"]
	if arch != "amd64" {
		return in
	}
	in.Level = 1
	for _, need := range levelFlags {
		for _, f := range need {
			if !flags[f] {
				return in
			}
		}
		in.Level++
	}
	return in
}

// detect is the platform-specific reader; the fallback claims nothing.
func detect() Info {
	if data, ok := readCPUInfo(); ok {
		return Parse(data, runtime.GOARCH)
	}
	return Info{Arch: runtime.GOARCH}
}
