package chat

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// A chunked upload must fit, whole, in the quotas and beside the uploads
// still under way, before its first piece is kept.
func TestChunkedUploadsAreHeldToQuotas(t *testing.T) {
	st, _ := testFedStore(t)
	svc := &Service{Store: st, Log: slog.New(slog.DiscardHandler), Bus: NewBus(), DataDir: t.TempDir(), Instance: func() (string, error) { return "us", nil }}
	room := &store.ChatRoom{ID: "R", GroupID: []byte("group-R"), Kind: KindGroup, Policy: DefaultPolicy(KindGroup).JSON(), ReplicaStatus: "live", CreatedBy: "fog:us/eve"}
	if err := st.CreateChatRoom(room); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatEvent(&store.ChatCommit{Event: &store.ChatEvent{RoomID: "R", ID: "c", Kind: EventSystem, Payload: []byte("{}")},
		Added: []store.ChatParticipant{{PersonAddr: "fog:us/eve", HomeInstance: "us", RoleIndex: RoleMember}}}); err != nil {
		t.Fatal(err)
	}
	actor := &Actor{Person: &store.Person{Handle: "eve"}, Addr: "fog:us/eve"}
	defer func(p, u int64) { QuotaPerPerson, QuotaUploads = p, u }(QuotaPerPerson, QuotaUploads)

	QuotaPerPerson = 100
	if _, _, err := svc.PutBlobChunk(actor, room, "up1", 0, 101, bytes.NewReader(make([]byte, 50))); !errors.Is(err, ErrQuota) {
		t.Errorf("an upload past the person's quota began: %v", err)
	}
	QuotaPerPerson, QuotaUploads = 1<<30, 120
	if _, have, err := svc.PutBlobChunk(actor, room, "up2", 0, 100, bytes.NewReader(make([]byte, 60))); err != nil || have != 60 {
		t.Fatalf("an upload within bounds: %d %v", have, err)
	}
	if _, _, err := svc.PutBlobChunk(actor, room, "up3", 0, 100, bytes.NewReader(make([]byte, 60))); !errors.Is(err, ErrQuota) {
		t.Errorf("a second upload past what may be under way began: %v", err)
	}
}

// A replica keeps a row for one of our people only for the identity key
// they hold: a row under another key (a deleted person whose handle was
// given again, a reset the hub hasn't heard of) gives whoever holds the
// address now nothing of the room.
func TestReplicaRowsAreForTheIdentityThatJoined(t *testing.T) {
	st, _ := testFedStore(t)
	fd := testFederation(st, nil)
	eve, _ := st.CreatePerson("eve", "Eve", store.PersonRoleMember)
	zed, _ := st.CreatePerson("zed", "Zed", store.PersonRoleMember)
	st.SetPersonIdentityKey(eve, []byte("eve-now-0123456789abcdef0123456"))
	st.SetPersonIdentityKey(zed, []byte("zed-key-0123456789abcdef0123456"))
	zedP, _ := st.PersonByHandle("zed")
	room := &store.ChatRoom{ID: "R", GroupID: []byte("group-R"), HubInstance: "hub", Kind: KindGroup, Policy: DefaultPolicy(KindGroup).JSON(), ReplicaStatus: "live"}
	if err := st.CreateChatRoom(room); err != nil {
		t.Fatal(err)
	}
	snap := &RoomSnapshot{Participants: []store.ChatParticipant{
		{PersonAddr: "fog:us/eve", RoleIndex: RoleAdmin, IdentityKey: "an-earlier-eve"},
		{PersonAddr: "fog:us/zed", RoleIndex: RoleMember, IdentityKey: identityOf(zedP)},
	}}
	st.AddChatInvite(&store.ChatInvite{RoomID: "R", PersonAddr: "fog:us/eve", InvitedBy: "x", Welcome: []byte("w")})
	st.AddChatInvite(&store.ChatInvite{RoomID: "R", PersonAddr: "fog:us/zed", InvitedBy: "x", Welcome: []byte("w")})
	got := map[string]int64{}
	for _, p := range fd.Svc.vetSnapshot(room, snap).Participants {
		got[p.PersonAddr] = p.LeftSeq
	}
	if got["fog:us/eve"] != -1 {
		t.Errorf("a row under an earlier identity key counts for the person now: leftSeq %d", got["fog:us/eve"])
	}
	if v, ok := got["fog:us/zed"]; !ok || v != 0 {
		t.Errorf("the identity that joined lost its row: %v", got)
	}
}

// errReader fails if anything reads it: a refusal must come before the body.
type errReader struct{ t *testing.T }

func (r errReader) Read([]byte) (int, error) {
	r.t.Error("the body was read before the quota was checked")
	return 0, errors.New("read")
}

// A chunked upload is held to the total it declared at its start, piece by
// piece; and a plain upload's declared length is checked before its body is
// read, and held to.
func TestUploadsAreHeldToWhatTheyDeclared(t *testing.T) {
	st, _ := testFedStore(t)
	svc := &Service{Store: st, Log: slog.New(slog.DiscardHandler), Bus: NewBus(), DataDir: t.TempDir(), Instance: func() (string, error) { return "us", nil }}
	room := &store.ChatRoom{ID: "R", GroupID: []byte("group-R"), Kind: KindGroup, Policy: DefaultPolicy(KindGroup).JSON(), ReplicaStatus: "live", CreatedBy: "fog:us/eve"}
	st.CreateChatRoom(room)
	st.AppendChatEvent(&store.ChatCommit{Event: &store.ChatEvent{RoomID: "R", ID: "c", Kind: EventSystem, Payload: []byte("{}")},
		Added: []store.ChatParticipant{{PersonAddr: "fog:us/eve", HomeInstance: "us", RoleIndex: RoleMember}}})
	actor := &Actor{Person: &store.Person{Handle: "eve"}, Addr: "fog:us/eve"}
	defer func(p, u int64) { QuotaPerPerson, QuotaUploads = p, u }(QuotaPerPerson, QuotaUploads)
	QuotaPerPerson, QuotaUploads = 100, 1<<30

	// Declared small, then grown piece by piece: refused.
	if _, have, err := svc.PutBlobChunk(actor, room, "u", 0, 50, bytes.NewReader(make([]byte, 40))); err != nil || have != 40 {
		t.Fatalf("start: %d %v", have, err)
	}
	if _, _, err := svc.PutBlobChunk(actor, room, "u", 40, 90, bytes.NewReader(make([]byte, 50))); !errors.Is(err, ErrBadInput) {
		t.Errorf("a piece declaring a larger total than the upload began with: %v", err)
	}
	// A reservation counts against the person: a second upload can't fit
	// beside it, though nothing of it is stored yet.
	if _, _, err := svc.PutBlobChunk(actor, room, "v", 0, 60, bytes.NewReader(make([]byte, 10))); !errors.Is(err, ErrQuota) {
		t.Errorf("a second upload past the person's quota beside a reservation: %v", err)
	}
	// A plain upload declaring past the quota is refused unread.
	if _, err := svc.PutBlob(actor, room, errReader{t}, 200); !errors.Is(err, ErrQuota) {
		t.Errorf("a declared length past the quota: %v", err)
	}
	// One that sends more than it declared is refused.
	if _, err := svc.PutBlob(actor, room, bytes.NewReader(make([]byte, 30)), 10); !errors.Is(err, ErrBadInput) {
		t.Errorf("a body longer than declared: %v", err)
	}
}
