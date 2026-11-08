package files

import (
	"strings"
	"testing"
)

func TestDisplayName(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"Ünïcödé 写真.jpg", "Ünïcödé 写真.jpg"},
		{"עברית.txt", "עברית.txt"}, // right-to-left letters are not controls
		{"a\nb.txt", "a␊b.txt"},
		{"a\r\x1b[31mred", "a␍␛[31mred"},
		{"nul\x00", "nul␀"},
		{"del\x7f", "del␡"},
		{"c1\u0085", `c1\x85`},
		{"bad\xffbyte", `bad\xFFbyte`},
		{"invoice‮fdp.exe", "invoice⟨U+202E⟩fdp.exe"},
		{"x⁦y⁩‏", "x⟨U+2066⟩y⟨U+2069⟩⟨U+200F⟩"},
	}
	for _, c := range cases {
		if got := DisplayName(c.in); got != c.want {
			t.Errorf("DisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Nothing that reorders or hides survives.
	out := DisplayName("a‪‫‬‭‮⁦⁧⁨⁩؜‎‏\x01\x7f")
	for _, r := range out {
		if r < 0x20 || r == 0x7f || isBidiControl(r) {
			t.Fatalf("%q survived in %q", r, out)
		}
	}
	if strings.Contains(out, "‮") {
		t.Fatal("override survived")
	}
}

// Characters that break a line or are not drawn at all are shown, so two
// names that look the same are told apart and a name stays on its line.
func TestDisplayNameShowsSeparatorsAndInvisibles(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"a\u2028b", "a⟨U+2028⟩b"},
		{"a\u2029b", "a⟨U+2029⟩b"},
		{"pay\u200Bpal.pdf", "pay⟨U+200B⟩pal.pdf"},
		{"pay\u200Cpal.pdf", "pay⟨U+200C⟩pal.pdf"}, // a joiner between ASCII hides
		{"pay\u200Dpal.pdf", "pay⟨U+200D⟩pal.pdf"},
		{"\u200Dx", "⟨U+200D⟩x"},
		{"x\u200D", "x⟨U+200D⟩"},
		{"a\u2060b\u2063c", "a⟨U+2060⟩b⟨U+2063⟩c"},
		{"\uFEFFbom.txt", "⟨U+FEFF⟩bom.txt"},
		{"a\u180Eb", "a⟨U+180E⟩b"},
		// Joiners doing their job are kept: an emoji sequence, Persian.
		{"👩\u200D💻.png", "👩\u200D💻.png"},
		{"می\u200Cخواهم.txt", "می\u200Cخواهم.txt"},
		// But not two in a row, nor next to something already shown.
		{"👩\u200D\u200D💻", "👩⟨U+200D⟩⟨U+200D⟩💻"},
		{"é\u200D\u202E", "é⟨U+200D⟩⟨U+202E⟩"},
	}
	for _, c := range cases {
		if got := DisplayName(c.in); got != c.want {
			t.Errorf("DisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
