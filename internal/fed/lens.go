package fed

// The consent grammar's shared vocabulary — the Go mirror of
// clients/fogfed/lib/fogfed/lens.rb. Three lens axes are the same words
// wherever sharing happens (fog's file shares, hub's records), so they live in
// one place per language and the conformance vectors prove the two agree term
// for term. The fourth axis, detail, is deliberately absent: it is
// kind-specific (files run full/previews/listing, records run field-group
// presets), so it is not shared vocabulary.

// LensTerm is one term of an axis: its wire value and a human label. The longer
// definitions live in docs/consent-grammar.md.
type LensTerm struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var (
	// LensModes — how far the peer may act.
	LensModes = []LensTerm{
		{Value: "read", Label: "read-only"},
		{Value: "contribute", Label: "read and contribute"},
	}
	// LensScopes — what of a collection crosses.
	LensScopes = []LensTerm{
		{Value: "all", Label: "everything"},
		{Value: "filtered", Label: "a filtered set"},
		{Value: "picked", Label: "a hand-picked set"},
	}
	// LensProvenance — how words carried across a peering are marked.
	LensProvenance = []LensTerm{
		{Value: "own", Label: "written here"},
		{Value: "firsthand", Label: "firsthand"},
		{Value: "withheld", Label: "withheld"},
	}
)

var lensAxes = map[string][]LensTerm{
	"mode": LensModes, "scope": LensScopes, "provenance": LensProvenance,
}

// LensValid reports whether value is a term of the named axis.
func LensValid(name, value string) bool {
	for _, t := range lensAxes[name] {
		if t.Value == value {
			return true
		}
	}
	return false
}
