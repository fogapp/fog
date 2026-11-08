package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/people"
)

// A person sees their sessions, can end one from another, and an ended
// session stops working at once. The operator can sign a person out of
// everything without touching their devices.
func TestChatSessionsListAndRevoke(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	dave := registerDevice(t, h, daveS, "dave")

	// The browser carries its own session and its chat credential; a chat
	// credential alone sees only devices (signin_handlers_device.go).
	list := decode(t, do(t, h, "GET", "/api/chat/me/sessions", "", withCookie(daveS), withCookie(dave.cookie)))
	sessions := list["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("dave should hold a web session and a device session: %v", sessions)
	}
	current := int64(list["current"].(float64))
	var other int64
	var deviceNamed bool
	for _, s := range sessions {
		m := s.(map[string]any)
		if int64(m["id"].(float64)) != current {
			other = int64(m["id"].(float64))
		}
		if m["deviceName"] == "dave's test device" {
			deviceNamed = true
		}
	}
	if current == 0 || other == 0 || !deviceNamed {
		t.Fatalf("session view: current=%d other=%d named=%v", current, other, deviceNamed)
	}
	// Revoke the web session from the browser's chat page; the web cookie dies.
	if w := do(t, h, "DELETE", "/api/chat/me/sessions/"+itoa(int(other)), "", withCookie(daveS), withCookie(dave.cookie)); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/chat/me", "", withCookie(daveS)); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked web session still works: %d", w.Code)
	}
	// The operator signs in through the same door as everyone else, so they
	// hold an ordinary person session and see it in their own list.
	if got := decode(t, do(t, h, "GET", "/api/chat/me/sessions", "", withCookie(admin))); len(got["sessions"].([]any)) != 1 || got["current"].(float64) == 0 {
		t.Errorf("operator sessions: %v", got)
	}
	// Someone else's session id is still not yours to end.
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	eveSess := int64(decode(t, do(t, h, "GET", "/api/chat/me/sessions", "", withCookie(eveS)))["current"].(float64))
	if w := do(t, h, "DELETE", "/api/chat/me/sessions/"+itoa(int(eveSess)), "", withCookie(dave.cookie)); w.Code != http.StatusNotFound {
		t.Errorf("cross-person revoke: %d", w.Code)
	}
	// The operator signs dave out everywhere; the device stays registered.
	// (A chat device's credential reaches chat alone: dave's id from there.)
	daveID := int(decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(dave.cookie)))["person"].(map[string]any)["id"].(float64))
	if w := do(t, h, "GET", "/api/people/"+itoa(daveID)+"/sessions", "", withCookie(admin)); w.Code != http.StatusOK || len(decode(t, w)["sessions"].([]any)) != 1 {
		t.Errorf("admin session list: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/people/"+itoa(daveID)+"/signout", "", withCookie(admin)); w.Code != http.StatusOK {
		t.Fatalf("signout: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/chat/me", "", withCookie(dave.cookie)); w.Code != http.StatusUnauthorized {
		t.Errorf("device session survived signout: %d", w.Code)
	}
	w := do(t, h, "POST", "/api/people/login", `{"handle":"dave","password":"correct horse battery"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login after signout: %d %s", w.Code, w.Body)
	}
	daveS2 := personCookie(t, w)
	me := decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(daveS2)))
	if n := len(me["devices"].([]any)); n != 1 {
		t.Errorf("devices after signout: %d", n)
	}
}

// Each person has one budget across the chat API; hitting it answers 429
// with Retry-After and does not touch anyone else's budget.
func TestChatPerPersonRateLimit(t *testing.T) {

	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	srv.chatLimiter = auth.NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if w := do(t, h, "GET", "/api/chat/me", "", withCookie(daveS)); w.Code != http.StatusOK {
			t.Fatalf("call %d: %d", i, w.Code)
		}
	}
	w := do(t, h, "GET", "/api/chat/me", "", withCookie(daveS))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("fourth call: %d %v", w.Code, w.Header())
	}
	if w := do(t, h, "GET", "/api/chat/me", "", withCookie(admin)); w.Code != http.StatusOK {
		t.Errorf("another person limited too: %d", w.Code)
	}
	// People endpoints outside chat are not on this budget.
	if w := do(t, h, "GET", "/api/people/me", "", withCookie(daveS)); w.Code != http.StatusOK {
		t.Errorf("people/me limited: %d", w.Code)
	}
}

// Cursors: limit is clamped, `before` pages backwards oldest-first, `after`
// pages forward, and nothing under a since-join floor ever leaves the hub.
func TestChatEventsPagination(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()

	gid, err := op.c.CreateGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Pages", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	// Twelve messages before dave exists in the room.
	for i := 0; i < 12; i++ {
		ct, err := op.c.SendMessage(ctx, gid, []byte("early"))
		if err != nil {
			t.Fatal(err)
		}
		if w := submit(t, h, op, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
			t.Fatalf("send %d: %d %s", i, w.Code, w.Body)
		}
	}
	commit, welcome, err := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "dave"))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", w.Code, w.Body)
	}
	joinSeq := int64(decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(op.cookie)))["room"].(map[string]any)["seq"].(float64))
	inv := decode(t, do(t, h, "GET", "/api/chat/invites", "", withCookie(dave.cookie)))["invites"].([]any)
	wb, _ := people.DecodeKey(inv[0].(map[string]any)["welcome"].(string))
	if _, err := dave.c.JoinGroup(ctx, wb); err != nil {
		t.Fatal(err)
	}
	do(t, h, "POST", "/api/chat/invites/"+itoa(int(inv[0].(map[string]any)["id"].(float64)))+"/accept", "", withCookie(dave.cookie))
	for i := 0; i < 5; i++ {
		ct, _ := op.c.SendMessage(ctx, gid, []byte("late"))
		submit(t, h, op, roomID, "mls_private", ct, nil)
	}
	seqs := func(w *httptest.ResponseRecorder) []int64 {
		var out []int64
		for _, e := range decode(t, w)["events"].([]any) {
			out = append(out, int64(e.(map[string]any)["seq"].(float64)))
		}
		return out
	}
	// The creator pages forward in threes and sees a contiguous ascending run.
	page := seqs(do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=0&limit=3", "", withCookie(op.cookie)))
	if len(page) != 3 || page[0] != 1 || page[2] != 3 {
		t.Errorf("first page: %v", page)
	}
	page = seqs(do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=3&limit=3", "", withCookie(op.cookie)))
	if len(page) != 3 || page[0] != 4 {
		t.Errorf("second page: %v", page)
	}
	// Backwards from the end, oldest-first inside the page.
	page = seqs(do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?before=6&limit=4", "", withCookie(op.cookie)))
	if len(page) != 4 || page[0] != 2 || page[3] != 5 {
		t.Errorf("before page: %v", page)
	}
	// Absurd limits and negatives are clamped, not errors.
	all := seqs(do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=-5&limit=100000", "", withCookie(op.cookie)))
	if len(all) != 19 { // creation + 12 + commit + 5
		t.Errorf("all events: %d", len(all))
	}
	// Groups are since-join: dave gets nothing before his commit, however
	// he pages, and nothing at all when he asks before the floor.
	if got := seqs(do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=0&limit=500", "", withCookie(dave.cookie))); len(got) == 0 || got[0] != joinSeq {
		t.Errorf("dave forward: %v (join %d)", got, joinSeq)
	}
	if got := seqs(do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?before="+itoa(int(joinSeq))+"&limit=50", "", withCookie(dave.cookie))); len(got) != 0 {
		t.Errorf("dave before floor: %v", got)
	}
	if got := seqs(do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?before=1000&limit=2", "", withCookie(dave.cookie))); len(got) != 2 || got[0] < joinSeq {
		t.Errorf("dave last two: %v", got)
	}
}
