package chat

import (
	"encoding/json"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// Which of our people a peer sees fails closed: a grant with no people, an
// unreadable one, or an unknown word shows nobody.
func TestUseConfigFailsClosed(t *testing.T) {
	for raw, want := range map[string]bool{
		``:           false,
		`""`:         false,
		`"everyone"`: false,
		`{"bad":1}`:  false,
		`42`:         false,
		`"members"`:  true,
		`"all"`:      true,
		`["dave"]`:   true,
		`["eve"]`:    false,
	} {
		if got := (UseConfig{People: json.RawMessage(raw)}).allows("dave"); got != want {
			t.Errorf("people %s: allows dave = %v, want %v", raw, got, want)
		}
	}
}

// The same at the source: the people.directory grant's config, as
// PeopleFor reads it to decide whom a peer sees. Missing, unreadable or
// empty means nobody.
func TestPeopleForFailsClosed(t *testing.T) {
	st, _ := testFedStore(t)
	if _, err := st.CreatePerson("dave", "Dave", store.PersonRoleMember); err != nil {
		t.Fatal(err)
	}
	peer, _ := st.CreatePairedPeer("them", "them", "http://them.invalid", "")
	fd := testFederation(st, nil)
	for cfg, want := range map[string]int{
		``:                      0,
		`not json`:              0,
		`{}`:                    0,
		`{"people":"everyone"}`: 0,
		`{"people":"members"}`:  1,
		`{"people":["dave"]}`:   1,
	} {
		if err := st.SetFedCapability(peer.ID, store.CapPeopleDirectory, store.DirGrant, cfg); err != nil {
			t.Fatal(err)
		}
		if got := len(fd.PeopleFor(peer)); got != want {
			t.Errorf("people.directory config %q: the peer sees %d people, want %d", cfg, got, want)
		}
	}
}
