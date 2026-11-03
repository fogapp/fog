package apps

import "testing"

// An app's Postgres role is its name with '-' as '_'. An app named so its role
// would be one of fog's own restore roles could be handed the role fog
// replays other apps' untrusted dumps as; such names are reserved.
func TestValidateNameReservesFogRoles(t *testing.T) {
	for _, bad := range []string{"fog-restore-scratch", "fog-scratch-0a1b2c", "fog-scratch-x"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q must be reserved: its role collides with fog's", bad)
		}
	}
	for _, ok := range []string{"hub", "fog-notes", "scratch", "restore-scratch"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q should be allowed: %v", ok, err)
		}
	}
}
