package apps

import "testing"

// On a case-insensitive Drive root ".FOG/trash" is fog's trash: a spec
// volume may not name it in any case, and neither may a host path into the
// default Drive root.
func TestReservedFilesRelIgnoresCase(t *testing.T) {
	for _, rel := range []string{".fog", ".FOG/trash", ".Fog/uploads", "apps/.FOG", "CONTENT/.fog/trash", "Content/.FoG"} {
		if err := ValidateFilesRel(rel); err == nil {
			t.Errorf("ValidateFilesRel(%q) accepted fog's reserved directory", rel)
		}
	}
	for _, rel := range []string{"Content/Books", "Docs/.FOG", "Apps/Paperless/.fog"} {
		if err := ValidateFilesRel(rel); err != nil {
			t.Errorf("ValidateFilesRel(%q) = %v", rel, err)
		}
	}
	if why := DangerousHostPath(DefaultDataDir+"/files/.FOG/trash", false); why == "" {
		t.Error("a host path into Drive's .FOG was allowed")
	}
	if why := DangerousHostPath(DefaultDataDir+"/files/Content/Books", false); why != "" {
		t.Errorf("an ordinary Drive folder by host path: %s", why)
	}
}
