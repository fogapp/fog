package flare

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Fingerprint computes the string that decides which group an event joins.
// The hierarchy (Sentry's, sized down): an explicit client fingerprint wins;
// then in-app stack frames; then the templated message. The single most
// important property: line numbers never participate — they churn on every
// deploy, and a fingerprint that includes them turns one bug into a new
// group per release.
func Fingerprint(ev *Event) string {
	if len(ev.Fingerprint) > 0 {
		// "{{ default }}" splices the computed grouping into a custom one, so
		// a client can subdivide a group (say, by tenant) without losing the
		// stack-based half.
		parts := make([]string, len(ev.Fingerprint))
		for i, p := range ev.Fingerprint {
			if strings.TrimSpace(p) == "{{ default }}" {
				parts[i] = defaultFingerprint(ev)
			} else {
				parts[i] = p
			}
		}
		return digest(strings.Join(parts, "\x00"))
	}
	return digest(defaultFingerprint(ev))
}

func defaultFingerprint(ev *Event) string {
	class := ev.ErrorClass()
	if c := ev.Crash(); c != nil && c.Stacktrace != nil && len(c.Stacktrace.Frames) > 0 {
		frames := inAppFrames(c.Stacktrace.Frames)
		if len(frames) == 0 {
			// Nothing in-app (error raised and rescued entirely inside a gem):
			// the innermost frames are the most stable proxy.
			frames = c.Stacktrace.Frames
			if len(frames) > 8 {
				frames = frames[len(frames)-8:]
			}
		}
		var b strings.Builder
		b.WriteString(class)
		for _, f := range frames {
			name := f.Filename
			if name == "" {
				name = f.AbsPath
			}
			b.WriteString("\x00" + name + "\x00" + f.Function)
		}
		return b.String()
	}
	// No stack at all: class + templated message, so "timeout connecting to
	// 10.0.3.7" and "…10.0.3.9" are one issue, not two.
	msg := ""
	if c := ev.Crash(); c != nil {
		msg = c.Value
	} else {
		msg = ev.Message.Formatted
	}
	return class + "\x00" + NormalizeMessage(msg)
}

func inAppFrames(frames []Frame) []Frame {
	var out []Frame
	for _, f := range frames {
		if f.InApp != nil && *f.InApp {
			out = append(out, f)
		}
	}
	return out
}

var (
	reQuoted = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	reHex    = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	reNum    = regexp.MustCompile(`\d+`)
)

// NormalizeMessage strips the volatile parts of an error message — quoted
// strings, long hex identifiers, numbers — so messages that differ only in
// their interpolated values template to the same fingerprint.
func NormalizeMessage(msg string) string {
	msg = reQuoted.ReplaceAllString(msg, "?")
	msg = reHex.ReplaceAllString(msg, "?")
	msg = reNum.ReplaceAllString(msg, "?")
	return strings.TrimSpace(msg)
}

func digest(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:16]) // 128 bits is plenty; half the column width
}
