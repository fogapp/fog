package fed

// The consent grammar's impact sentence, shared with the fogfed gem's
// Fogfed::Impact so fog and every app read a share back the same way. See
// clients/fogfed/lib/fogfed/impact.rb; the conformance vectors prove the two
// agree.

import "strings"

// ToSentence joins a list the way the sentence reads: "", "a", "a and b",
// "a, b, and c" (Oxford comma) — matching Ruby's Array#to_sentence.
func ToSentence(items []string) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it != "" {
			out = append(out, it)
		}
	}
	switch len(out) {
	case 0:
		return ""
	case 1:
		return out[0]
	case 2:
		return out[0] + " and " + out[1]
	default:
		return strings.Join(out[:len(out)-1], ", ") + ", and " + out[len(out)-1]
	}
}

// ImpactSentence assembles the impact sentence from a lens's parts:
//
//	<subject> sees <scope> — <sees>.
//	[ They do not see <hidden>.]
//	[ Never shared: <floor>.]
//	[ <addendum>.]
func ImpactSentence(subject, scope string, sees, hidden []string, floor, addendum string) string {
	var b strings.Builder
	b.WriteString(subject + " sees " + scope + " — " + ToSentence(sees) + ".")
	if h := ToSentence(hidden); h != "" {
		b.WriteString(" They do not see " + h + ".")
	}
	if floor != "" {
		b.WriteString(" Never shared: " + floor + ".")
	}
	if addendum != "" {
		b.WriteString(" " + addendum + ".")
	}
	return b.String()
}
