// Moving a room's hub between households: the owner's request on the old
// hub, and the federation side the new hub answers.
package server

import (
	"net/http"
	"strings"
)

// handleChatMigrate: POST /api/chat/rooms/{id}/migrate {"to": "<instance>"}.
func (s *Server) handleChatMigrate(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.chat.Fed.Migrate(r.Context(), actor, room, strings.TrimSpace(req.To)); err != nil {
		s.chatError(w, err)
		return
	}
	fresh, _ := s.store.ChatRoom(room.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hubInstance": fresh.HubInstance, "seq": fresh.Seq})
}

// handleFedChatAdopt: the old hub asks this fog to take over a room it
// already follows, frozen at the given seq.
func (s *Server) handleFedChatAdopt(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	var req struct {
		Seq   int64  `json:"seq"`
		Epoch int64  `json:"epoch"`
		By    string `json:"by"`
	}
	if !fedDecode(w, body, &req) {
		return
	}
	seq, err := s.chat.Fed.Adopt(r.Context(), peer, r.PathValue("id"), req.Seq, req.By)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "adopt_failed", "seq": seq})
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"seq": seq})
}

// handleFedChatBlobList lists the attachments a room hubbed here holds, for
// a new hub copying them across.
func (s *Server) handleFedChatBlobList(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	room := s.fedRoomPresent(w, r, peer)
	if room == nil {
		return
	}
	shas, err := s.store.ChatBlobsForRoom(room.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if shas == nil {
		shas = []string{}
	}
	s.writeFedSignedJSON(w, r, map[string]any{"blobs": shas})
}
