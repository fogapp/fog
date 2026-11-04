package apps

import "slices"

// FormerNames is software that changed its name, each name it shipped under
// before. A fog-backup/1 artifact declares the software that made it (the
// manifest's "app"), so an artifact made under an old name still belongs to
// the software — Fin was Manor until 2026-11, and a Manor backup restores
// into a fresh Fin. Never the reverse: older software can't read what its
// renamed self wrote.
var FormerNames = map[string][]string{
	"fin": {"manor"},
}

// SameSoftware reports whether an artifact declaring declared belongs in an
// app that runs identity.
func SameSoftware(declared, identity string) bool {
	return declared == identity || slices.Contains(FormerNames[identity], declared)
}
