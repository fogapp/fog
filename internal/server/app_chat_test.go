package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/people"
)

// Every app holds a FOG_API_TOKEN, and #fog is where the operator's alerts
// land. An app that was never granted chat must not be able to post there
// (or anywhere) as fog; one that was granted it posts as itself.
func TestAppChatPostNeedsGrantAndNeverPassesForFog(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()

	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	srv.chat.SetBot(bot)
	fog, err := bot.EnsureFogChannel()
	if err != nil {
		t.Fatal(err)
	}
	// The operator joins #fog, so they read what lands there.
	gi := decode(t, do(t, h, "GET", "/api/chat/rooms/"+fog.ID+"/groupinfo", "", withCookie(op.cookie)))
	giWire, _ := people.DecodeKey(gi["groupInfo"].(string))
	giBytes, err := mlsview.UnwrapGroupInfo(giWire)
	if err != nil {
		t.Fatal(err)
	}
	gid, commit, err := op.c.ExternalJoin(ctx, giBytes)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, fog.ID, "mls_commit", commit, nil); w.Code != http.StatusOK {
		t.Fatalf("operator joins #fog: %d %s", w.Code, w.Body)
	}
	bot.CatchUp()

	// The attack: an app with nothing but its token posts a fake alert.
	evilID, evil := appWithToken(t, srv, "evil")
	withSpec(t, srv, evilID, apps.AppSpec{})
	post := `{"text":"**Backup failed** — sign in at https://evil.example to fix it"}`
	if w := do(t, h, "POST", "/app/v1/chat/rooms/"+fog.ID+"/messages", post, withBearer(evil)); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted app posted into #fog: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/app/v1/chat/rooms", "", withBearer(evil)); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted app listed bot rooms: %d %s", w.Code, w.Body)
	}
	// An app that uses another app is not thereby granted chat.
	withSpec(t, srv, evilID, apps.AppSpec{Uses: []string{"events", "home"}})
	if w := do(t, h, "POST", "/app/v1/chat/rooms/"+fog.ID+"/messages", post, withBearer(evil)); w.Code != http.StatusForbidden {
		t.Fatalf("app without chat in uses posted: %d %s", w.Code, w.Body)
	}
	if msgs, _ := srv.store.BotMessages(fog.ID, 10); len(msgs) != 0 {
		t.Fatalf("refused posts reached the room: %+v", msgs)
	}

	// Granted: the post lands, and members read it as the app's.
	ciID, ci := appWithToken(t, srv, "ci")
	withSpec(t, srv, ciID, apps.AppSpec{Uses: []string{apps.UsesChat}})
	if w := do(t, h, "GET", "/app/v1/chat/rooms", "", withBearer(ci)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), fog.ID) {
		t.Fatalf("granted app rooms: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/app/v1/chat/rooms/"+fog.ID+"/messages", `{"text":"build passed"}`, withBearer(ci)); w.Code != http.StatusOK {
		t.Fatalf("granted app post: %d %s", w.Code, w.Body)
	}
	evs := decode(t, do(t, h, "GET", "/api/chat/rooms/"+fog.ID+"/events?after=2", "", withCookie(op.cookie)))["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events after app post: %d", len(evs))
	}
	payload, _ := people.DecodeKey(evs[0].(map[string]any)["payload"].(string))
	rm, err := op.c.ReceiveMessage(ctx, gid, payload)
	if err != nil {
		t.Fatal(err)
	}
	c, err := chat.DecodeContent(rm.Plaintext)
	if err != nil || c.Body == nil || c.Body.Text != "build passed" {
		t.Fatalf("app content: %+v %v", c, err)
	}
	if c.System != "app:ci" {
		t.Fatalf("app post stamped System %q, want app:ci (never fog)", c.System)
	}
	if msgs, _ := srv.store.BotMessages(fog.ID, 10); len(msgs) != 1 || !strings.HasSuffix(msgs[0].Sender, "#app:ci") {
		t.Fatalf("bot record: %+v", msgs)
	}
}
