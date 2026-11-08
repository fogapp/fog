package store

import "testing"

func TestNasShareCRUD(t *testing.T) {
	t.Parallel()

	s := testStore(t)

	id, err := s.CreateNasShare(NasShare{
		Name: "media", Proto: "nfs", Server: "nas.lan",
		Remote: "/volume1/media", MountPoint: "/mnt/nas/media", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("create: zero id")
	}

	// Duplicate name and duplicate mount point both violate UNIQUE.
	if _, err := s.CreateNasShare(NasShare{Name: "media", Proto: "cifs", Server: "x", Remote: "y", MountPoint: "/mnt/other"}); err == nil {
		t.Error("duplicate name accepted")
	}
	if _, err := s.CreateNasShare(NasShare{Name: "other", Proto: "cifs", Server: "x", Remote: "y", MountPoint: "/mnt/nas/media"}); err == nil {
		t.Error("duplicate mount point accepted")
	}

	sh, err := s.GetNasShare("media")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !sh.Enabled || sh.Proto != "nfs" || sh.MountPoint != "/mnt/nas/media" {
		t.Errorf("round trip mismatch: %+v", sh)
	}

	if err := s.SetNasShareEnabled("media", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if sh, _ = s.GetNasShare("media"); sh.Enabled {
		t.Error("still enabled after disable")
	}

	list, err := s.ListNasShares()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v, n=%d", err, len(list))
	}

	if err := s.DeleteNasShare("media"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetNasShare("media"); err == nil {
		t.Error("share still present after delete")
	}
}
