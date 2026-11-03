package store

import (
	"slices"
	"testing"
)

// The batched read answers, per ref, what PersonByRef, PersonPrefs and
// PushSubsForPerson answer one person at a time: refs by handle or uid,
// unset prefs as "{}", revoked devices' subscriptions left out, and
// unknown refs absent.
func TestNotifyRecipientsMatchesPerPersonReads(t *testing.T) {
	s := testStore(t)
	ann, _ := s.CreatePerson("ann", "Ann", PersonRoleMember)
	bob, _ := s.CreatePerson("bob", "Bob", PersonRoleMember)
	if err := s.SetPersonPrefs(ann, `{"pushEnabled":true}`); err != nil {
		t.Fatal(err)
	}
	annPhone, _ := s.CreatePersonDevice(ann, "phone", "web", []byte("k1"), nil)
	annOld, _ := s.CreatePersonDevice(ann, "old", "web", []byte("k2"), nil)
	bobPhone, _ := s.CreatePersonDevice(bob, "phone", "web", []byte("k3"), nil)
	for _, d := range []struct {
		id       int64
		endpoint string
	}{{annPhone, "https://push/a1"}, {annOld, "https://push/a2"}, {bobPhone, "https://push/b1"}} {
		if err := s.PutPushSub(d.id, d.endpoint, "p", "a"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RevokeDevice(annOld); err != nil {
		t.Fatal(err)
	}
	bobRow, _ := s.PersonByID(bob)

	refs := []string{"ann", bobRow.UID, "nobody", "01ARZ3NDEKTSV4RRFFQ69G5FAV"}
	got, err := s.NotifyRecipients(refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d recipients, want 2: %+v", len(got), got)
	}
	for _, ref := range refs[:2] {
		p, err := s.PersonByRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		prefs, _ := s.PersonPrefs(p.ID)
		subs, _ := s.PushSubsForPerson(p.ID)
		r := got[ref]
		if r.PersonID != p.ID || r.Prefs != prefs {
			t.Errorf("%s: got %+v, want person %d prefs %q", ref, r, p.ID, prefs)
		}
		if !slices.Equal(r.Subs, subs) {
			t.Errorf("%s: subs %+v, want %+v", ref, r.Subs, subs)
		}
	}
	if got[bobRow.UID].Prefs != "{}" {
		t.Errorf("unset prefs = %q, want {}", got[bobRow.UID].Prefs)
	}
	if subs := got["ann"].Subs; len(subs) != 1 || subs[0].Endpoint != "https://push/a1" {
		t.Errorf("ann's subs = %+v, want only the live phone's", subs)
	}
	if empty, err := s.NotifyRecipients(nil); err != nil || len(empty) != 0 {
		t.Errorf("no refs: %v, %v", empty, err)
	}
}

// Everyone's live devices at once, as PersonDevices lists each person's.
func TestAllPersonDevicesMatchesPersonDevices(t *testing.T) {
	s := testStore(t)
	ann, _ := s.CreatePerson("ann", "Ann", PersonRoleMember)
	bob, _ := s.CreatePerson("bob", "Bob", PersonRoleMember)
	cat, _ := s.CreatePerson("cat", "Cat", PersonRoleMember) // no devices
	s.CreatePersonDevice(ann, "a1", "web", []byte("k1"), nil)
	b1, _ := s.CreatePersonDevice(bob, "b1", "web", []byte("k2"), nil)
	s.CreatePersonDevice(ann, "a2", "web", []byte("k3"), []byte("att"))
	s.CreatePersonDevice(bob, "b2", "web", []byte("k4"), nil)
	if err := s.RevokeDevice(b1); err != nil {
		t.Fatal(err)
	}

	all, err := s.AllPersonDevices()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{ann, bob, cat} {
		one, _ := s.PersonDevices(id)
		if len(all[id]) != len(one) {
			t.Fatalf("person %d: all %+v, one %+v", id, all[id], one)
		}
		for i := range one {
			if all[id][i].ID != one[i].ID || all[id][i].Name != one[i].Name || string(all[id][i].Attestation) != string(one[i].Attestation) {
				t.Errorf("person %d device %d: all %+v, one %+v", id, i, all[id][i], one[i])
			}
		}
	}
	if _, ok := all[cat]; ok {
		t.Error("a person with no devices has an entry")
	}
	if len(all[bob]) != 1 || all[bob][0].Name != "b2" {
		t.Errorf("bob's devices = %+v, want only b2 (b1 is revoked)", all[bob])
	}
}
