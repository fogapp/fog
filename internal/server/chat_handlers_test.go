package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	mls "github.com/thomas-vilte/mls-go"
	"github.com/thomas-vilte/mls-go/ciphersuite"
	"github.com/thomas-vilte/mls-go/keypackages"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/people"
)

// A device in these tests is a real mls-go client whose signature key is the
// device key fog knows — the same shape the browser SDK has.
type testDevice struct {
	handle string
	addr   string
	c      *mls.Client
	cookie *http.Cookie
	id     int64
}

func ulid(t *testing.T) string {
	t.Helper()
	id, err := fed.NewULID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// testIdentities holds the identity keys registerDevice minted, by person
// address, so it can attest each person's later devices as the SDK's
// identity-holding device does.
var testIdentities sync.Map // addr → ed25519.PrivateKey

// registerDevice creates an MLS client for the person and registers its
// signature key as a device with one published KeyPackage, returning a
// device-bound session cookie. Like the SDK's first device, it sets the
// person's identity key if they have none and attests the device with it;
// a person whose identity key the test set itself gets an unverified one.
func registerDevice(t *testing.T, h http.Handler, session *http.Cookie, handle string) *testDevice {
	t.Helper()
	me := decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(session)))
	inst := me["household"].(map[string]any)["instanceId"].(string)
	if inst == "" {
		t.Fatal("no instance id")
	}
	addr := "fog:" + inst + "/" + handle
	if me["identityKey"] == "" {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		if w := do(t, h, "PUT", "/api/chat/me/identity", `{"identityKey":"`+people.EncodeKey(pub)+`"}`, withCookie(session)); w.Code != http.StatusOK {
			t.Fatalf("set identity: %d %s", w.Code, w.Body)
		}
		testIdentities.Store(addr, priv)
	}
	var idPriv ed25519.PrivateKey
	if v, ok := testIdentities.Load(addr); ok {
		k := v.(ed25519.PrivateKey)
		if idk, _ := people.DecodeKey(fmtString(me["identityKey"])); me["identityKey"] == "" || string(idk) == string(k.Public().(ed25519.PublicKey)) {
			idPriv = k
		}
	}
	c, err := mls.NewClient([]byte(addr), ciphersuite.MLS128DHKEMX25519)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := c.FreshKeyPackageBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := keypackages.UnmarshalKeyPackage(kp)
	if err != nil {
		t.Fatal(err)
	}
	reg := map[string]any{
		"name": handle + "'s test device", "deviceKey": people.EncodeKey(pkg.LeafNode.SignatureKeyBytes),
		"keyPackages": []string{people.EncodeKey(kp)},
	}
	if idPriv != nil {
		reg["attestation"] = people.EncodeKey(ed25519.Sign(idPriv, people.AttestationMessage(pkg.LeafNode.SignatureKeyBytes)))
	}
	body, _ := json.Marshal(reg)
	w := do(t, h, "POST", "/api/chat/me/devices", string(body), withCookie(session))
	if w.Code != http.StatusOK {
		t.Fatalf("register device: %d %s", w.Code, w.Body)
	}
	d := decode(t, w)["device"].(map[string]any)
	return &testDevice{handle: handle, addr: addr, c: c, cookie: personCookie(t, w), id: int64(d["id"].(float64))}
}

func fmtString(v any) string {
	s, _ := v.(string)
	return s
}

// groupInfoOf is d's GroupInfo for a group, with its ratchet tree, as the
// hub takes it (base64url MLSMessage).
func groupInfoOf(t *testing.T, d *testDevice, gid []byte) string {
	t.Helper()
	gi, err := d.c.GroupInfo(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	return people.EncodeKey(mlsview.WrapGroupInfo(gi))
}

// submit posts one MLS message into a room.
func submit(t *testing.T, h http.Handler, d *testDevice, room, kind string, payload, welcome []byte) *httptest.ResponseRecorder {
	t.Helper()
	in := map[string]any{"id": ulid(t), "kind": kind, "payload": people.EncodeKey(payload), "sentAt": time.Now().UTC().Format(time.RFC3339)}
	if welcome != nil {
		in["welcome"] = people.EncodeKey(welcome)
	}
	// Every commit carries the GroupInfo of the epoch it makes, as the SDK
	// sends it: the committer's, once its client has the commit applied.
	if kind == "mls_commit" {
		if msg, err := mlsview.Parse(payload); err == nil {
			if gi, err := d.c.GroupInfo(context.Background(), msg.GroupID); err == nil {
				in["groupInfo"] = people.EncodeKey(mlsview.WrapGroupInfo(gi))
			}
		}
	}
	body, _ := json.Marshal(in)
	return do(t, h, "POST", "/api/chat/rooms/"+room+"/events", string(body), withCookie(d.cookie))
}

// keysFor fetches a person's key material and returns the first device's
// KeyPackage.
func keysFor(t *testing.T, h http.Handler, as *http.Cookie, handle string) []byte {
	t.Helper()
	km := decode(t, do(t, h, "GET", "/api/chat/people/"+handle+"/keys", "", withCookie(as)))
	devs := km["devices"].([]any)
	if len(devs) == 0 {
		t.Fatalf("%s has no devices", handle)
	}
	kpS, ok := devs[0].(map[string]any)["keyPackage"].(string)
	if !ok {
		t.Fatalf("%s has no key package left", handle)
	}
	kp, _ := people.DecodeKey(kpS)
	return kp
}

func TestChatRoomLifecycle(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	eve := registerDevice(t, h, eveS, "eve")
	ctx := context.Background()

	// The operator creates a group locally, then the room on the hub with
	// the same group id.
	gid, err := op.c.CreateGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Family", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("create room: %d %s", w.Code, w.Body)
	}
	room := decode(t, w)["room"].(map[string]any)
	roomID := room["id"].(string)
	if room["myRole"].(float64) != 4 || room["participants"].(float64) != 1 {
		t.Errorf("creator view: %v", room)
	}

	// A session without a device cannot create or post.
	if w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(daveS)); w.Code != http.StatusPreconditionRequired {
		t.Errorf("deviceless create: %d %s", w.Code, w.Body)
	}

	// Add dave: fetch his key package, commit, post commit + welcome.
	daveKP := keysFor(t, h, op.cookie, "dave")
	commit, welcome, err := op.c.InviteMember(ctx, gid, daveKP)
	if err != nil {
		t.Fatal(err)
	}
	w = submit(t, h, op, roomID, "mls_commit", commit, welcome)
	if w.Code != http.StatusOK {
		t.Fatalf("commit add dave: %d %s", w.Code, w.Body)
	}
	ev := decode(t, w)["event"].(map[string]any)
	if ev["seq"].(float64) != 2 || ev["kind"] != "mls_commit" {
		t.Errorf("commit event: %v", ev)
	}
	detail := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(op.cookie)))
	if detail["room"].(map[string]any)["epoch"].(float64) != 1 {
		t.Errorf("epoch after add: %v", detail["room"].(map[string]any)["epoch"])
	}
	if n := len(detail["leaves"].([]any)); n != 2 {
		t.Errorf("leaves after add: %d", n)
	}

	// Dave sees the invite with the Welcome and joins.
	inv := decode(t, do(t, h, "GET", "/api/chat/invites", "", withCookie(dave.cookie)))["invites"].([]any)
	if len(inv) != 1 {
		t.Fatalf("dave's invites: %v", inv)
	}
	wb, _ := people.DecodeKey(inv[0].(map[string]any)["welcome"].(string))
	if _, err := dave.c.JoinGroup(ctx, wb); err != nil {
		t.Fatalf("dave join: %v", err)
	}
	do(t, h, "POST", "/api/chat/invites/"+itoa(int(inv[0].(map[string]any)["id"].(float64)))+"/accept", "", withCookie(dave.cookie))
	rooms := decode(t, do(t, h, "GET", "/api/chat/rooms", "", withCookie(dave.cookie)))
	if len(rooms["rooms"].([]any)) != 1 || len(rooms["invites"].([]any)) != 0 {
		t.Errorf("dave's rooms after accept: %v", rooms)
	}

	// Dave sends; the operator reads the ciphertext from the log and decrypts.
	ct, err := dave.c.SendMessage(ctx, gid, []byte("hello from dave"))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, dave, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatalf("dave message: %d %s", w.Code, w.Body)
	}
	evs := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=2", "", withCookie(op.cookie)))["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events after 2: %d", len(evs))
	}
	payload, _ := people.DecodeKey(evs[0].(map[string]any)["payload"].(string))
	rm, err := op.c.ReceiveMessage(ctx, gid, payload)
	if err != nil || string(rm.Plaintext) != "hello from dave" {
		t.Fatalf("operator decrypt: %v %+v", err, rm)
	}
	// The stored payload is ciphertext: the plaintext is nowhere in it.
	if strings.Contains(string(payload), "hello from dave") {
		t.Fatal("plaintext leaked into the log")
	}

	// Eve is not in the room: she cannot read or post.
	if w := do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events", "", withCookie(eve.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("eve reading: %d", w.Code)
	}
	if w := submit(t, h, eve, roomID, "mls_private", ct, nil); w.Code != http.StatusForbidden {
		t.Errorf("eve posting: %d %s", w.Code, w.Body)
	}

	// Replaying the same event id returns the original, not a second row.
	in := map[string]any{"id": ev["id"], "kind": "mls_commit", "payload": people.EncodeKey(commit)}
	b, _ := json.Marshal(in)
	w = do(t, h, "POST", "/api/chat/rooms/"+roomID+"/events", string(b), withCookie(op.cookie))
	if w.Code != http.StatusOK || decode(t, w)["replayed"] != true {
		t.Errorf("replay: %d %s", w.Code, w.Body)
	}

	// Two commits race: dave's key refresh (a commit of nothing but his
	// path) lands, the operator's stale add of eve is refused with the epoch
	// to sync to.
	upd, _, err := dave.c.CommitPendingProposals(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, dave, roomID, "mls_commit", upd, nil); w.Code != http.StatusOK {
		t.Fatalf("self-update: %d %s", w.Code, w.Body)
	}
	eveKP := keysFor(t, h, op.cookie, "eve")
	stale, _, err := op.c.InviteMember(ctx, gid, eveKP)
	if err != nil {
		t.Fatal(err)
	}
	w = submit(t, h, op, roomID, "mls_commit", stale, nil)
	if w.Code != http.StatusConflict || decode(t, w)["code"] != "epoch_stale" {
		t.Errorf("stale commit: %d %s", w.Code, w.Body)
	}

	// A member cannot remove someone else: policy forbids it at the hub.
	// (dave's client applies the removal locally; the hub never does.)
	rmc, err := dave.c.RemoveMember(ctx, gid, []byte(op.addr))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, dave, roomID, "mls_commit", rmc, nil); w.Code != http.StatusForbidden {
		t.Errorf("member removing owner: %d %s", w.Code, w.Body)
	}

	// Read marks and unread counts.
	do(t, h, "POST", "/api/chat/rooms/"+roomID+"/read", `{"seq":3}`, withCookie(op.cookie))
	rv := decode(t, do(t, h, "GET", "/api/chat/rooms", "", withCookie(op.cookie)))["rooms"].([]any)[0].(map[string]any)
	if rv["readSeq"].(float64) != 3 || rv["unread"].(float64) != 1 {
		t.Errorf("unread: read %v unread %v seq %v", rv["readSeq"], rv["unread"], rv["seq"])
	}
}

func TestChatChannelExternalJoin(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "channel", "label": "general", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("create channel: %d %s", w.Code, w.Body)
	}
	roomID := decode(t, w)["room"].(map[string]any)["id"].(string)

	// Dave sees the channel as available, and joinable from the start: the
	// creator's GroupInfo came with the room.
	rooms := decode(t, do(t, h, "GET", "/api/chat/rooms", "", withCookie(dave.cookie)))
	avail := rooms["available"].([]any)
	if len(avail) != 1 || avail[0].(map[string]any)["joinable"] != true {
		t.Fatalf("available: %v", avail)
	}

	// The operator publishes group info for the current epoch.
	gi, err := op.c.GroupInfo(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"epoch": 0, "groupInfo": people.EncodeKey(mlsview.WrapGroupInfo(gi))})
	if w := do(t, h, "PUT", "/api/chat/rooms/"+roomID+"/groupinfo", string(b), withCookie(op.cookie)); w.Code != http.StatusOK {
		t.Fatalf("publish groupinfo: %d %s", w.Code, w.Body)
	}

	// Dave fetches it and joins externally; the hub sees a new_member_commit
	// from a device it knows, in an open channel of its own household.
	giRes := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/groupinfo", "", withCookie(dave.cookie)))
	giWire, _ := people.DecodeKey(giRes["groupInfo"].(string))
	giBytes, err := mlsview.UnwrapGroupInfo(giWire)
	if err != nil {
		t.Fatal(err)
	}
	_, commit, err := dave.c.ExternalJoin(ctx, giBytes)
	if err != nil {
		t.Fatal(err)
	}
	w = submit(t, h, dave, roomID, "mls_commit", commit, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("external join: %d %s", w.Code, w.Body)
	}
	if err := op.c.ProcessCommit(ctx, gid, commit); err != nil {
		t.Fatalf("operator processing dave's join: %v", err)
	}
	detail := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(dave.cookie)))
	if detail["room"].(map[string]any)["participants"].(float64) != 2 {
		t.Errorf("participants after join: %v", detail["room"])
	}
	// Messages flow both ways afterwards.
	ct, _ := op.c.SendMessage(ctx, gid, []byte("welcome dave"))
	if w := submit(t, h, op, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatalf("post after join: %d %s", w.Code, w.Body)
	}
	got, err := dave.c.ReceiveMessage(ctx, gid, ct)
	if err != nil || string(got.Plaintext) != "welcome dave" {
		t.Fatalf("dave decrypt: %v", err)
	}
	// A channel with shared history shows dave the room from the start.
	evs := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events", "", withCookie(dave.cookie)))["events"].([]any)
	if len(evs) != 3 || evs[0].(map[string]any)["kind"] != "system" {
		t.Errorf("dave's history: %d events", len(evs))
	}
}

func TestChatStreamDeliversLive(t *testing.T) {

	srv, h := newTestServer(t)
	_ = srv
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Us", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	commit, welcome, _ := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "dave"))
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add dave: %s", w.Body)
	}
	dave.c.JoinGroup(ctx, welcome)

	ts := httptest.NewServer(sameTestClient(h)) // one client in-process and over the socket
	defer ts.Close()
	hdr := http.Header{}
	hdr.Add("Cookie", dave.cookie.Name+"="+dave.cookie.Value)
	wsctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/chat/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.Write(wsctx, websocket.MessageText, []byte(`{"since":{}}`))

	read := func() streamFrame {
		_, data, err := conn.Read(wsctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var f streamFrame
		json.Unmarshal(data, &f)
		return f
	}
	// Replay: a group's history is since-join, so dave sees the commit that
	// added him but not the creation before it; then synced.
	var kinds []string
	for {
		f := read()
		kinds = append(kinds, f.Kind)
		if f.Kind == "synced" {
			if f.Cursors[roomID] != 2 {
				t.Errorf("cursor after replay: %v", f.Cursors)
			}
			break
		}
	}
	if len(kinds) != 2 {
		t.Errorf("replay frames: %v", kinds)
	}
	// Live: the operator posts, dave's stream carries it.
	ct, _ := op.c.SendMessage(ctx, gid, []byte("live"))
	if w := submit(t, h, op, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatalf("post: %s", w.Body)
	}
	for {
		f := read()
		if f.Kind == "event" && f.Event != nil && f.Event.Kind == "mls_private" {
			if got, err := dave.c.ReceiveMessage(ctx, gid, f.Event.Payload); err != nil || string(got.Plaintext) != "live" {
				t.Fatalf("live decrypt: %v", err)
			}
			break
		}
	}
}

func TestChatBlobs(t *testing.T) {

	srv, h := newTestServer(t)
	admin := setup(t, h)
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()
	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Us", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)

	r := httptest.NewRequest("POST", "/api/chat/blobs?room="+roomID, strings.NewReader("encrypted-bytes"))
	r.AddCookie(op.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("put blob: %d %s", w.Code, w.Body)
	}
	sha := decode(t, w)["sha256"].(string)
	w = do(t, h, "GET", "/api/chat/blobs/"+sha, "", withCookie(op.cookie))
	if w.Code != http.StatusOK || w.Body.String() != "encrypted-bytes" {
		t.Errorf("get blob: %d %q", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/chat/blobs/"+sha, "", withCookie(eveS)); w.Code != http.StatusForbidden {
		t.Errorf("non-participant blob read: %d", w.Code)
	}

	// The hub can't see which sealed message names a blob, so an old blob
	// survives the sweep until the room's retention ages it out.
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`UPDATE chat_blobs SET stored_at = ? WHERE sha256 = ?`, old, sha); err != nil {
		t.Fatal(err)
	}
	srv.chat.PruneHistory(time.Now())
	if w := do(t, h, "GET", "/api/chat/blobs/"+sha, "", withCookie(op.cookie)); w.Code != http.StatusOK {
		t.Fatalf("blob swept without retention: %d", w.Code)
	}
	room, _ := srv.store.ChatRoom(roomID)
	pol := chat.DefaultPolicy(room.Kind)
	pol.Retention.Days = 1
	pj, _ := json.Marshal(pol)
	if _, err := srv.store.DB().Exec(`UPDATE chat_rooms SET policy = ? WHERE id = ?`, string(pj), roomID); err != nil {
		t.Fatal(err)
	}
	srv.chat.PruneHistory(time.Now())
	if w := do(t, h, "GET", "/api/chat/blobs/"+sha, "", withCookie(op.cookie)); w.Code == http.StatusOK {
		t.Errorf("blob outlived retention: %d", w.Code)
	}
}

// A person's second device may not let itself into their rooms while it is
// unverified — that is the ghost-device attack. Once the person attests it,
// it may; and a fresh session can bind to a device by proving its key.
func TestChatUnverifiedDeviceCannotSelfJoin(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()

	// Dave's first device sets an identity and attests itself.
	idPub, idPriv, _ := ed25519.GenerateKey(rand.Reader)
	do(t, h, "PUT", "/api/chat/me/identity", `{"identityKey":"`+people.EncodeKey(idPub)+`"}`, withCookie(daveS))
	phone := registerDevice(t, h, daveS, "dave")
	phoneKey := mustDeviceKey(t, h, phone)
	att := ed25519.Sign(idPriv, people.AttestationMessage(phoneKey))
	if w := do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(phone.id))+"/attest", `{"attestation":"`+people.EncodeKey(att)+`"}`, withCookie(daveS)); w.Code != http.StatusOK {
		t.Fatalf("attest phone: %s", w.Body)
	}

	// The operator opens a channel and adds dave's phone.
	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "channel", "label": "general", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	commit, welcome, _ := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "dave"))
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add dave: %s", w.Body)
	}
	phone.c.JoinGroup(ctx, welcome)
	gi, _ := op.c.GroupInfo(ctx, gid)
	giB, _ := json.Marshal(map[string]any{"epoch": 1, "groupInfo": people.EncodeKey(mlsview.WrapGroupInfo(gi))})
	do(t, h, "PUT", "/api/chat/rooms/"+roomID+"/groupinfo", string(giB), withCookie(op.cookie))

	// A second, unattested device of dave's tries an external join: refused,
	// from the group info it would join by onwards.
	tablet := registerDevice(t, h, daveS, "dave")
	if w := do(t, h, "GET", "/api/chat/rooms/"+roomID+"/groupinfo", "", withCookie(tablet.cookie)); w.Code != http.StatusForbidden {
		t.Fatalf("unverified device fetching group info: %d %s", w.Code, w.Body)
	}
	giRes := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/groupinfo", "", withCookie(phone.cookie)))
	giWire, _ := people.DecodeKey(giRes["groupInfo"].(string))
	giRaw, _ := mlsview.UnwrapGroupInfo(giWire)
	_, ext, err := tablet.c.ExternalJoin(ctx, giRaw)
	if err != nil {
		t.Fatal(err)
	}
	w := submit(t, h, tablet, roomID, "mls_commit", ext, nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not verified") {
		t.Fatalf("unverified self-join: %d %s", w.Code, w.Body)
	}

	// Attested by the identity key, it may join on its own.
	tabletKey := mustDeviceKey(t, h, tablet)
	att2 := ed25519.Sign(idPriv, people.AttestationMessage(tabletKey))
	if w := do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(tablet.id))+"/attest", `{"attestation":"`+people.EncodeKey(att2)+`"}`, withCookie(daveS)); w.Code != http.StatusOK {
		t.Fatalf("attest tablet: %s", w.Body)
	}
	// The tablet's earlier external commit is stale now only if epochs
	// moved; nothing did, so the same commit lands.
	if w := submit(t, h, tablet, roomID, "mls_commit", ext, nil); w.Code != http.StatusOK {
		t.Fatalf("verified self-join: %d %s", w.Code, w.Body)
	}

	// Binding a fresh session to a device by proving its key: a laptop
	// registered with a key this test holds.
	lapPub, lapPriv, _ := ed25519.GenerateKey(rand.Reader)
	w = do(t, h, "POST", "/api/chat/me/devices", `{"name":"laptop","deviceKey":"`+people.EncodeKey(lapPub)+`"}`, withCookie(daveS))
	if w.Code != http.StatusOK {
		t.Fatalf("register laptop: %s", w.Body)
	}
	lapID := itoa(int(decode(t, w)["device"].(map[string]any)["id"].(float64)))
	ch := decode(t, do(t, h, "GET", "/api/chat/me/devices/"+lapID+"/challenge", "", withCookie(daveS)))
	nonce := ch["nonce"].(string)
	// A wrong key is refused, and consumes the nonce.
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	bad := ed25519.Sign(wrongPriv, []byte(people.DeviceSessionMessage(nonce)))
	if w := do(t, h, "POST", "/api/chat/me/devices/"+lapID+"/session", `{"nonce":"`+nonce+`","signature":"`+people.EncodeKey(bad)+`"}`, withCookie(daveS)); w.Code != http.StatusForbidden {
		t.Fatalf("wrong key bound a session: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/chat/me/devices/"+lapID+"/session", `{"nonce":"`+nonce+`","signature":"`+people.EncodeKey(ed25519.Sign(lapPriv, []byte(people.DeviceSessionMessage(nonce))))+`"}`, withCookie(daveS)); w.Code != http.StatusBadRequest {
		t.Fatalf("reused nonce accepted: %d %s", w.Code, w.Body)
	}
	ch = decode(t, do(t, h, "GET", "/api/chat/me/devices/"+lapID+"/challenge", "", withCookie(daveS)))
	nonce = ch["nonce"].(string)
	sig := ed25519.Sign(lapPriv, []byte(people.DeviceSessionMessage(nonce)))
	w = do(t, h, "POST", "/api/chat/me/devices/"+lapID+"/session", `{"nonce":"`+nonce+`","signature":"`+people.EncodeKey(sig)+`","clientKind":"native"}`, withCookie(daveS))
	if w.Code != http.StatusOK {
		t.Fatalf("bind session: %d %s", w.Code, w.Body)
	}
	token := decode(t, w)["token"].(string)
	r := httptest.NewRequest("GET", "/api/chat/me", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if me := decode(t, rec); itoa(int(me["deviceId"].(float64))) != lapID {
		t.Fatalf("bound session device: %v", me["deviceId"])
	}
}

// mustDeviceKey reads a test device's registered key back from the server.
func mustDeviceKey(t *testing.T, h http.Handler, d *testDevice) []byte {
	t.Helper()
	me := decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(d.cookie)))
	for _, x := range me["devices"].([]any) {
		dm := x.(map[string]any)
		if int64(dm["id"].(float64)) == d.id {
			k, _ := people.DecodeKey(dm["deviceKey"].(string))
			return k
		}
	}
	t.Fatalf("device %d not listed", d.id)
	return nil
}
