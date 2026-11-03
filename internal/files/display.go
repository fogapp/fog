package files

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// DisplayName is a Drive name made safe to show to a person. The name on
// disk is whatever an app wrote, and two kinds of character in it can lie
// about what the file is:
//
//   - control characters (a newline, a carriage return, an escape) break a
//     line or a terminal, or vanish; each is shown as its Control Pictures
//     symbol (␊, ␍, ␛, ␡) or, for the C1 range and bytes that are not
//     UTF-8, as \xNN;
//   - bidirectional controls (U+202A–202E, U+2066–2069, and the LRM, RLM
//     and ALM marks) reorder what follows them, so "invoice‮fdp.exe"
//     reads as "invoiceexe.pdf"; each is shown as ⟨U+202E⟩ and does nothing;
//   - the line and paragraph separators (U+2028, U+2029) break a line as a
//     newline does, and invisible characters (zero-width space U+200B,
//     word joiner and invisible operators U+2060–2064, U+FEFF, U+180E)
//     make two different names look identical: each is shown as ⟨U+…⟩
//     too. The zero-width joiner and non-joiner (U+200D, U+200C) are part
//     of how emoji sequences and scripts such as Persian are written, so
//     they are kept between two non-ASCII characters and shown only where
//     they sit next to ASCII or at an end, where they can only hide.
//
// It is for display only. Every operation — rename, delete, download, the
// Content-Disposition filename (RFC 5987-encoded by mime.FormatMediaType) —
// keeps using the real name. The frontend's displayName (lib/displayName.ts)
// is the same function.
func DisplayName(name string) string {
	if !needsDisplay(name) {
		return name
	}
	var b strings.Builder
	prev := rune(-1)
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		switch {
		case isJoiner(r) && joins(prev, name[i+size:]):
			b.WriteRune(r)
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02X`, name[i])
		case r < 0x20:
			b.WriteRune(0x2400 + r)
		case r == 0x7F:
			b.WriteRune('␡')
		case r >= 0x80 && r <= 0x9F:
			fmt.Fprintf(&b, `\x%02X`, r)
		case isBidiControl(r), isInvisible(r), isJoiner(r):
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
		default:
			b.WriteRune(r)
		}
		if !(r == utf8.RuneError && size == 1) {
			prev = r
		} else {
			prev = -1
		}
		i += size
	}
	return b.String()
}

// joins says a joiner between prev and the start of rest is doing a
// joiner's job: both neighbours are non-ASCII characters.
func joins(prev rune, rest string) bool {
	next, size := utf8.DecodeRuneInString(rest)
	return prev >= 0x80 && next >= 0x80 && !(next == utf8.RuneError && size <= 1) &&
		!isUnsafeRune(prev) && !isUnsafeRune(next)
}

func isJoiner(r rune) bool { return r == 0x200C || r == 0x200D }

// isInvisible is a character that is not drawn, or that breaks a line.
func isInvisible(r rune) bool {
	return r == 0x200B || (r >= 0x2060 && r <= 0x2064) || r == 0xFEFF || r == 0x180E || r == 0x2028 || r == 0x2029
}

// isUnsafeRune is every character DisplayName rewrites unconditionally.
func isUnsafeRune(r rune) bool {
	return r < 0x20 || (r >= 0x7F && r <= 0x9F) || isBidiControl(r) || isInvisible(r) || isJoiner(r)
}

func needsDisplay(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if isUnsafeRune(r) {
			return true
		}
	}
	return false
}

func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}
