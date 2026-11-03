// Chat, the Slack-shaped rest (docs/chat-contract.md Phase 4): a person's
// preferences, Web Push subscriptions, link previews, incoming webhooks, and
// the app-facing surface — the last two through the fog bot, into rooms
// people put it in.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- preferences ----

func (s *Server) handleChatPrefs(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	raw, err := s.store.PersonPrefs(pc.Person.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chat.ParsePrefs(raw))
}

func (s *Server) handleChatPutPrefs(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	var prefs chat.Prefs
	if !readJSON(w, r, &prefs) {
		return
	}
	prefs.NtfyTopic = strings.TrimSpace(prefs.NtfyTopic)
	if prefs.NtfyTopic != "" && !notify.ValidTopic(prefs.NtfyTopic) {
		badRequest(w, "ntfy topic may hold only letters, digits, - and _ (at most 64)")
		return
	}
	// Where the person's alerts go — sign-ins and credential changes among
	// them — is the browser's own, freshly proven (signin_handlers_device.go):
	// moving the ntfy topic, or turning push off. Turning push on only adds
	// a channel, and a device may.
	if raw, err := s.store.PersonPrefs(pc.Person.ID); err == nil {
		if was := chat.ParsePrefs(raw); (was.NtfyTopic != prefs.NtfyTopic || (was.PushEnabled && !prefs.PushEnabled)) && !s.freshBrowser(w, r) {
			return
		}
	}
	if err := s.store.SetPersonPrefs(pc.Person.ID, prefs.JSON()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// ---- Web Push ----

func (s *Server) handleChatVAPID(w http.ResponseWriter, r *http.Request) {
	if s.chat.Notify == nil {
		writeErr(w, http.StatusServiceUnavailable, "push is not available on this fog")
		return
	}
	pub, err := s.chat.Notify.VAPIDPublicKey()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": pub})
}

func (s *Server) handleChatPushSubscribe(w http.ResponseWriter, r *http.Request) {
	d := s.ownDevice(w, r)
	if d == nil || !s.mayChangePush(w, r, d.ID) { // signin_handlers_device.go
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !strings.HasPrefix(req.Endpoint, "https://") || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		badRequest(w, "a push subscription needs an https endpoint and keys")
		return
	}
	// Fog will dial the endpoint: never one on its own network.
	if err := chat.CheckPublicURL(req.Endpoint, true); err != nil {
		badRequest(w, "a push endpoint must be a public https address")
		return
	}
	if err := s.store.PutPushSub(d.ID, req.Endpoint, req.Keys.P256dh, req.Keys.Auth); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

func (s *Server) handleChatPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	d := s.ownDevice(w, r)
	if d == nil || !s.mayChangePush(w, r, d.ID) {
		return
	}
	if err := s.store.DeletePushSubs(d.ID); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

// ---- link previews ----

// handleChatPreview fetches a page's metadata for the sender's device, which
// then encrypts it into the message. Fog sees the link; the person opted in.
func (s *Server) handleChatPreview(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	raw, _ := s.store.PersonPrefs(pc.Person.ID)
	if !chat.ParsePrefs(raw).Previews {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "link previews are off for this person", "code": "previews_off"})
		return
	}
	u := r.URL.Query().Get("url")
	if u == "" {
		badRequest(w, "url required")
		return
	}
	pv, err := chat.FetchPreview(r.Context(), u, nil, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

// ---- webhooks ----

func (s *Server) handleChatWebhooks(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	if _, err := s.chat.Participant(room, actor.Addr); err != nil {
		s.chatError(w, err)
		return
	}
	hooks, _ := s.store.ChatWebhooks(room.ID)
	if hooks == nil {
		hooks = []store.ChatWebhook{}
	}
	botIn := s.chat.Bot() != nil && s.chat.Bot().InRoom(room.ID)
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": hooks, "botInRoom": botIn, "botAddr": s.botAddr()})
}

func (s *Server) botAddr() string {
	if s.chat.Bot() == nil {
		return ""
	}
	return s.chat.Bot().Addr()
}

func (s *Server) handleChatCreateWebhook(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	part, err := s.chat.Participant(room, actor.Addr)
	if err != nil {
		s.chatError(w, err)
		return
	}
	pol, _ := chat.ParsePolicy(room.Policy)
	if !pol.Can(part.RoleIndex, chat.CapChangePolicy) {
		s.chatError(w, chat.ErrForbidden)
		return
	}
	if s.chat.Bot() == nil || !s.chat.Bot().InRoom(room.ID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "add fog to this room first — a webhook posts as fog, and fog can only post where it is a member", "code": "bot_not_in_room"})
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 40 {
		badRequest(w, "name required (up to 40 characters)")
		return
	}
	token, digest := auth.NewToken()
	id, err := s.store.CreateChatWebhook(room.ID, name, digest, actor.Addr)
	if err != nil {
		writeError(w, err)
		return
	}
	s.auditAs(actor.Person.Handle, "chat.webhook.create", room.ID, map[string]any{"name": name})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "url": s.publicURL(r) + "/hooks/chat/" + token})
}

func (s *Server) handleChatDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	part, err := s.chat.Participant(room, actor.Addr)
	if err != nil {
		s.chatError(w, err)
		return
	}
	pol, _ := chat.ParsePolicy(room.Policy)
	if !pol.Can(part.RoleIndex, chat.CapChangePolicy) {
		s.chatError(w, chat.ErrForbidden)
		return
	}
	id, ok := pathInt64(w, r, "hook")
	if !ok {
		return
	}
	if err := s.store.DeleteChatWebhook(id, room.ID); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

// handleChatHook is the public inbound webhook: token in the path, a JSON
// {"text": …} or a plain-text body, posted by the bot into the hook's room.
func (s *Server) handleChatHook(w http.ResponseWriter, r *http.Request) {
	// A wrong token is charged to the address, among strangers; a real
	// one to its hook (signin_handlers_limits.go).
	hook, err := s.store.ChatWebhookByDigest(auth.DigestToken(r.PathValue("token")))
	if err != nil {
		if s.limitStranger(w, r, "chat-hook", "rate limited") {
			notFound(w, "not found")
		}
		return
	}
	if !limit(w, s.hookLimiter, "hook:"+strconv.FormatInt(hook.ID, 10), "this webhook is posting too fast") {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		badRequest(w, "body too large")
		return
	}
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var j struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(body, &j) == nil && j.Text != "" {
			text = j.Text
		}
	}
	if text == "" {
		badRequest(w, "nothing to post")
		return
	}
	if len(text) > 8000 {
		text = text[:8000] + "…"
	}
	if s.chat.Bot() == nil || !s.chat.Bot().InRoom(hook.RoomID) {
		conflict(w, "fog is not in this room any more")
		return
	}
	if _, err := s.chat.Bot().SendAs(hook.RoomID, text, "webhook:"+hook.Name); err != nil {
		s.chatError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// ---- sessions (device tokens) ----

func (s *Server) handleChatSessions(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	rows, err := s.store.ListPersonSessions(pc.Person.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	// A chat credential or a device's token sees only devices' sessions
	// (signin_handlers_device.go).
	rows = s.visibleSessions(r, rows)
	if rows == nil {
		rows = []store.PersonSessionView{}
	}
	var current int64
	if pc.Session != nil {
		current = pc.Session.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": rows, "current": current})
}

func (s *Server) handleChatRevokeSession(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// Only a session this caller may see: a chat credential or a device's
	// token never ends a browser session (signin_handlers_device.go).
	rows, _ := s.store.ListPersonSessions(pc.Person.ID)
	if !slices.ContainsFunc(s.visibleSessions(r, rows), func(v store.PersonSessionView) bool { return v.ID == id }) {
		notFound(w, "no such session")
		return
	}
	if err := s.store.DeletePersonSession(id, pc.Person.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, "no such session")
			return
		}
		writeError(w, err)
		return
	}
	s.audit(r, "people.session.revoke", pc.Person.Handle, map[string]any{"session": id})
	writeOK(w)
}

func (s *Server) handleAdminPersonSessions(w http.ResponseWriter, r *http.Request) {
	p := s.personFromPath(w, r)
	if p == nil {
		return
	}
	rows, err := s.store.ListPersonSessions(p.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == nil {
		rows = []store.PersonSessionView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": rows})
}

// handleAdminPersonSignout ends every session a person holds: the lever for
// a lost phone when the person cannot get in themselves. Devices stay
// registered; the person signs in again and their devices keep their keys.
func (s *Server) handleAdminPersonSignout(w http.ResponseWriter, r *http.Request) {
	p := s.personFromPath(w, r)
	if p == nil {
		return
	}
	if err := s.store.DeletePersonSessions(p.ID); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "people.signout", p.Handle, nil)
	writeOK(w)
}

// ---- apps: /app/v1/chat ----

// appChatAuthed authenticates an app for /app/v1/chat. FOG_API_TOKEN alone
// is not enough: every app holds one, and a post lands in rooms people read
// as fog's, #fog among them. The app's spec must carry "chat" in its uses,
// which the operator granted by installing or saving it.
func (s *Server) appChatAuthed(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return nil, false
	}
	spec, ok := s.appSpec(w, app)
	if !ok {
		return nil, false
	}
	if !spec.UsesGrant(apps.UsesChat) {
		forbidden(w, `chat needs "chat" in this app's uses`)
		return nil, false
	}
	return app, true
}

func (s *Server) handleAppChatRooms(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.appChatAuthed(w, r); !ok {
		return
	}
	out := []map[string]any{}
	if s.chat.Bot() != nil {
		for _, id := range s.chat.Bot().Rooms() {
			if room, err := s.store.ChatRoom(id); err == nil && room.ArchivedAt == "" {
				out = append(out, map[string]any{"id": room.ID, "kind": room.Kind, "label": room.Label, "topic": room.TopicLabel})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Server) handleAppChatPost(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appChatAuthed(w, r)
	if !ok {
		return
	}
	if !limit(w, s.hookLimiter, "app:"+app.Name, "this app is posting too fast") {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		badRequest(w, "text required")
		return
	}
	if len(text) > 8000 {
		text = strings.ToValidUTF8(text[:8000], "") + "…"
	}
	roomID := r.PathValue("id")
	if s.chat.Bot() == nil || !s.chat.Bot().InRoom(roomID) {
		forbidden(w, "fog is not in that room; someone in it must add fog first")
		return
	}
	ev, err := s.chat.Bot().SendAs(roomID, text, "app:"+app.Name)
	if err != nil {
		s.chatError(w, err)
		return
	}
	s.auditAs("app:"+app.Name, "chat.app.post", roomID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"seq": ev.Seq, "id": ev.ID})
}
