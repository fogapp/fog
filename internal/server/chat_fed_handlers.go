// Chat across households: the signed /fed/v1/chat plane
// (docs/chat-contract.md). Every handler follows the files plane's shape —
// fedAuthed, then the
// chat grant, then a signed answer — and the hub-side ones add one more
// check: a fog may only speak for its own people.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// fedChatPeer authenticates the caller and requires the chat grant.
func (s *Server) fedChatPeer(w http.ResponseWriter, r *http.Request, maxBody int64) (*store.FedPeer, []byte, bool) {
	peer, body, ok := s.fedAuthed(w, r, maxBody)
	if !ok {
		return nil, nil, false
	}
	if _, granted, _ := s.store.FedCapabilityConfig(peer.ID, chat.CapChat, store.DirGrant); !granted {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this peer is not granted chat here", "code": "forbidden"})
		return nil, nil, false
	}
	s.store.TouchFedPeerSeen(peer.ID)
	return peer, body, true
}

// remoteActor builds the Actor a follower speaks for: a person of the
// calling household, from a device that household advertises. The person
// and device rows are synthetic — they live on the peer — but the address
// and key are exactly what the hub's checks need.
func (s *Server) remoteActor(w http.ResponseWriter, peer *store.FedPeer, addr, deviceKey string) *chat.Actor {
	home, handle, ok := chat.ParseAddr(addr)
	if !ok || home != peer.InstanceID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a household may only act for its own people", "code": "forbidden"})
		return nil
	}
	// One person, one address in rooms: the handle form their household
	// advertises, never their ULID alias.
	if c, err := s.chat.CanonicalAddr(addr); err != nil || c != addr {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "act for a person by the address their household advertises", "code": "forbidden"})
		return nil
	}
	var key []byte
	if deviceKey != "" {
		k, err := people.DecodeKey(deviceKey)
		if err != nil {
			badRequest(w, "malformed device key")
			return nil
		}
		key = k
	}
	person := &store.Person{Handle: handle, DisplayName: handle, Status: store.PersonStatusActive}
	for _, p := range chat.PeerPeople(peer) {
		if p.Names(handle) {
			person.DisplayName = p.DisplayName
			// The identity key their household advertises now: who they
			// are, whatever their address (remoteRoomActor).
			person.IdentityKey, _ = people.DecodeKey(p.IdentityKey)
		}
	}
	actor := &chat.Actor{Person: person, Addr: addr}
	if key != nil {
		actor.Device = &store.PersonDevice{DeviceKey: key, Name: "remote"}
	}
	return actor
}

// remoteRoomActor is remoteActor for a route on one room (by its path id):
// in a room hubbed here, someone acting under a participant's address with
// an identity key other than the one that participant joined with is
// someone else — a new person under a reused handle, or a reset their hub
// never heard of — and is counted out of the room and refused.
func (s *Server) remoteRoomActor(w http.ResponseWriter, r *http.Request, peer *store.FedPeer, addr, deviceKey string) *chat.Actor {
	actor := s.remoteActor(w, peer, addr, deviceKey)
	if actor == nil {
		return nil
	}
	if room, err := s.store.ChatRoom(r.PathValue("id")); err == nil && room.HubInstance == "" {
		if err := s.chat.SameIdentity(room, actor); err != nil {
			s.chatError(w, err)
			return nil
		}
	}
	return actor
}

// fedRoomHere resolves a room hubbed on this fog that the calling household
// has a stake in (chat.Federation.Stake): someone in it or invited to it
// (cap 0), or — once all its people have gone — only the log up to where the
// last of them left (cap > 0).
func (s *Server) fedRoomHere(w http.ResponseWriter, r *http.Request, peer *store.FedPeer) (*store.ChatRoom, int64) {
	room, err := s.store.ChatRoom(r.PathValue("id"))
	if err != nil || room.HubInstance != "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such room here", "code": "not_found"})
		return nil, 0
	}
	cap, ok := s.chat.Fed.Stake(room, peer.InstanceID)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "that household has no one in this room", "code": "forbidden"})
		return nil, 0
	}
	return room, cap
}

// fedRoomPresent is fedRoomHere for routes only a household with someone
// still in the room (or invited) may use.
func (s *Server) fedRoomPresent(w http.ResponseWriter, r *http.Request, peer *store.FedPeer) *store.ChatRoom {
	room, cap := s.fedRoomHere(w, r, peer)
	if room == nil {
		return nil
	}
	if cap != 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "that household has no one in this room any more", "code": "forbidden"})
		return nil
	}
	return room
}

// ---- key material and directory ----

func (s *Server) handleFedChatPersonKeys(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	// The person's handle, or — from a peer already on person addresses —
	// their ULID (docs/signin-contract.md).
	ref := r.PathValue("handle")
	if _, err := people.LookupHandle(ref); err != nil && !isPersonULID(ref) {
		badRequest(w, err.Error())
		return
	}
	p, err := s.store.PersonByRef(ref)
	if err != nil || p.Status != store.PersonStatusActive {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such person", "code": "not_found"})
		return
	}
	// The use-side config decides which people this peer may see.
	visible := false
	for _, dp := range s.chat.Fed.PeopleFor(peer) {
		if dp.Handle == p.Handle {
			visible = true
		}
	}
	if !visible {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such person", "code": "not_found"})
		return
	}
	cs, _ := strconv.Atoi(r.URL.Query().Get("ciphersuite"))
	if cs == 0 {
		cs = DefaultCiphersuite
	}
	s.auditAs(store.FederationActor(peer.Name), "chat.keys.served", p.Handle, nil)
	consume := s.chat.Keys.Take(time.Now(), chat.PeerRules(peer.ID, p.Handle)...)
	s.writeFedSignedJSON(w, r, s.keyMaterialFor(p, cs, 0, consume))
}

// ---- hub side: followers relaying for their people ----

func (s *Server) handleFedChatSubmit(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, int64(chat.MaxPayload)*3)
	if !ok {
		return
	}
	room := s.fedRoomPresent(w, r, peer)
	if room == nil {
		return
	}
	var req struct {
		Actor     string           `json:"actor"`
		DeviceKey string           `json:"deviceKey"`
		Input     chat.SubmitInput `json:"input"`
	}
	if !fedDecode(w, body, &req) {
		return
	}
	actor := s.remoteRoomActor(w, r, peer, req.Actor, req.DeviceKey)
	if actor == nil {
		return
	}
	res, err := s.chat.Submit(actor, room, req.Input)
	if err != nil {
		if errors.Is(err, chat.ErrEpochStale) {
			s.writeEpochStale(w, room, err)
			return
		}
		s.chatError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"event": res.Event, "replayed": res.Replayed})
}

func (s *Server) handleFedChatRoom(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	room, _ := s.fedRoomHere(w, r, peer)
	if room == nil {
		return
	}
	snap, err := s.chat.Fed.SnapshotFor(room, peer.InstanceID)
	if err != nil {
		writeError(w, err)
		return
	}
	body, err := json.Marshal(snap)
	if err != nil {
		writeError(w, err)
		return
	}
	// Tagged by content, so a follower's idle re-sync of a room nothing has
	// happened in is a signed 304. A follower too old to send If-None-Match
	// ignores the header.
	etag := chat.SnapshotETag(body)
	if s.writeFedNotModified(w, r, etag) {
		return
	}
	s.writeFedSignedBytes(w, r, body)
}

func (s *Server) handleFedChatEvents(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	room, cap := s.fedRoomHere(w, r, peer)
	if room == nil {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	evs, err := s.store.ChatEventsAfter(room.ID, after, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	seq, epoch := room.Seq, room.Epoch
	if cap != 0 {
		// A household with no one left reads up to where its last person
		// left, and learns nothing of the room since.
		kept := evs[:0]
		for _, e := range evs {
			if e.Seq <= cap {
				kept = append(kept, e)
			}
		}
		evs, seq, epoch = kept, cap, 0
	}
	if evs == nil {
		evs = []store.ChatEvent{}
	}
	s.writeFedSignedJSON(w, r, map[string]any{"events": evs, "seq": seq, "epoch": epoch})
}

func (s *Server) handleFedChatGroupInfo(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	room := s.fedRoomPresent(w, r, peer)
	if room == nil {
		return
	}
	actor := s.remoteRoomActor(w, r, peer, r.URL.Query().Get("actor"), r.URL.Query().Get("deviceKey"))
	if actor == nil {
		return
	}
	info, epoch, err := s.chat.GroupInfoFor(actor, room)
	if err != nil {
		s.chatError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"groupInfo": people.EncodeKey(info), "epoch": epoch})
}

func (s *Server) handleFedChatPutGroupInfo(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, int64(chat.MaxPayload)*2)
	if !ok {
		return
	}
	room := s.fedRoomPresent(w, r, peer)
	if room == nil {
		return
	}
	var req struct {
		Actor     string `json:"actor"`
		Epoch     int64  `json:"epoch"`
		GroupInfo string `json:"groupInfo"`
	}
	if !fedDecode(w, body, &req) {
		return
	}
	actor := s.remoteRoomActor(w, r, peer, req.Actor, "")
	if actor == nil {
		return
	}
	info, err := people.DecodeKey(req.GroupInfo)
	if err != nil {
		badRequest(w, "malformed group info")
		return
	}
	if err := s.chat.SetGroupInfo(actor, room, req.Epoch, info); err != nil {
		s.chatError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true})
}

func (s *Server) handleFedChatBlob(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	// A follower serves its cached copy to its own hub: after a hand-over
	// the new hub copies the old hub's blobs across, and by then the old hub
	// is a follower of the new one.
	var room *store.ChatRoom
	var cap int64
	if rm, err := s.store.ChatRoom(r.PathValue("id")); err == nil && rm.HubInstance != "" && rm.HubInstance == peer.InstanceID {
		room = rm
	} else if room, cap = s.fedRoomHere(w, r, peer); room == nil {
		return
	}
	sha := r.PathValue("sha")
	b, err := s.store.ChatBlob(sha)
	if err != nil || b.RoomID != room.ID || !chat.BlobVisible(b, cap) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such blob", "code": "not_found"})
		return
	}
	f, err := s.chat.OpenBlobRaw(sha)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such blob", "code": "not_found"})
		return
	}
	defer f.Close()
	if b.RoomSeq > 0 {
		w.Header().Set(chat.BlobSeqHeader, strconv.FormatInt(b.RoomSeq, 10))
	}
	// Streamed, signed by its digest and sealed to the caller, like any
	// file on the fed plane (serveFedFile).
	s.serveFedFile(w, r, f, b.Size, b.SHA256)
	// Served to the room's current hub, which checks it by digest as it
	// stores it: from now this copy is a cache, and may be evicted.
	if room.HubInstance == peer.InstanceID {
		s.store.ConfirmChatBlobHandedOff(sha)
	}
}

func (s *Server) handleFedChatPutBlob(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, int64(chat.BlobChunk)+1)
	if !ok {
		return
	}
	room := s.fedRoomPresent(w, r, peer)
	if room == nil {
		return
	}
	actor := s.remoteRoomActor(w, r, peer, r.URL.Query().Get("actor"), "")
	if actor == nil {
		return
	}
	q := r.URL.Query()
	if upload := q.Get("upload"); upload != "" && q.Get("abort") != "" {
		// The follower gave up on an upload: its reservation goes now.
		s.chat.AbortBlobChunks(actor, room, upload)
		s.writeFedSignedJSON(w, r, map[string]any{"ok": true})
		return
	}
	if upload := q.Get("upload"); upload != "" {
		// A piece of a large attachment (chat.BlobChunk at a time).
		offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		total, _ := strconv.ParseInt(q.Get("total"), 10, 64)
		b, have, err := s.chat.PutBlobChunk(actor, room, upload, offset, total, bytesReader(body))
		if err != nil {
			s.chatError(w, err)
			return
		}
		if b == nil {
			s.writeFedSignedJSON(w, r, map[string]any{"received": have})
			return
		}
		s.writeFedSignedJSON(w, r, map[string]any{"sha256": b.SHA256, "size": b.Size})
		return
	}
	b, err := s.chat.PutBlob(actor, room, bytesReader(body), int64(len(body)))
	if err != nil {
		s.chatError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"sha256": b.SHA256, "size": b.Size})
}

// handleFedChatUpdate is a follower relaying one of its people's changes to
// a room hubbed here (labels, a role, archiving, leaving), or telling the hub
// an operator reset one of its people's identity. The hub's rules apply as
// to its own people; the follower's replica takes the result from the next
// snapshot.
func (s *Server) handleFedChatUpdate(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, 1<<16)
	if !ok {
		return
	}
	room := s.fedRoomPresent(w, r, peer)
	if room == nil {
		return
	}
	var req struct {
		Actor         string         `json:"actor"`
		Patch         chat.RoomPatch `json:"patch"`
		IdentityReset bool           `json:"identityReset"`
		// DeviceKey names the actor's device, for a device leaving by itself.
		DeviceKey string `json:"deviceKey"`
	}
	if !fedDecode(w, body, &req) {
		return
	}
	// An identity reset is about the key changing; anything else is acted
	// on only for the identity that joined.
	var actor *chat.Actor
	if req.IdentityReset {
		actor = s.remoteActor(w, peer, req.Actor, "")
	} else {
		actor = s.remoteRoomActor(w, r, peer, req.Actor, req.DeviceKey)
	}
	if actor == nil {
		return
	}
	var err error
	if req.IdentityReset {
		err = s.chat.RemoteIdentityReset(actor, room)
	} else {
		err = s.chat.UpdateRoom(actor, room, req.Patch)
	}
	if err != nil {
		s.chatError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true})
}

// ---- follower side: what the hub pushes ----

func (s *Server) handleFedChatNotify(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, 8<<20)
	if !ok {
		return
	}
	var env struct {
		Items []json.RawMessage `json:"items"`
	}
	if !fedDecode(w, body, &env) {
		return
	}
	refused := s.chat.Fed.ReceiveRaw(r.Context(), peer, env.Items)
	if refused == nil {
		refused = []map[string]string{}
	}
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true, "refused": refused})
}

// handleFedChatDepartures takes a household's word that its people left
// rooms hubbed here (an identity reset, a deletion): by its signature, not
// by its directory, which a deleted person is no longer in.
func (s *Server) handleFedChatDepartures(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, 1<<20)
	if !ok {
		return
	}
	items, err := chat.DecodeDepartures(body)
	if err != nil {
		badRequest(w, "malformed departures")
		return
	}
	moved, retry := s.chat.Fed.ReceiveDepartures(peer, items)
	if moved == nil {
		moved = []map[string]string{}
	}
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true, "moved": moved, "retry": retry})
}

func (s *Server) handleFedChatEphemeral(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedChatPeer(w, r, 1<<12)
	if !ok {
		return
	}
	var req struct {
		RoomID string `json:"roomId"`
		Person string `json:"person"`
		Signal string `json:"signal"`
	}
	if !fedDecode(w, body, &req) {
		return
	}
	room, err := s.store.ChatRoom(req.RoomID)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// On a follower the hub relays everyone's typing; on the hub a
	// follower reports its own person's. Either way the person must be in
	// the room, and a follower may only report its own people.
	if room.HubInstance == "" {
		if home, _, ok := chat.ParseAddr(req.Person); !ok || home != peer.InstanceID {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	} else if room.HubInstance != peer.InstanceID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := s.chat.Participant(room, req.Person); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch req.Signal {
	case "typing", "presence:online", "presence:offline":
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.chat.Bus.PublishRoom(room.ID, chat.Notification{Kind: "ephemeral", RoomID: room.ID, Person: req.Person, Signal: req.Signal}, nil)
	if room.HubInstance == "" {
		go s.chat.Fed.SendEphemeral(room, req.Person, req.Signal)
	}
	w.WriteHeader(http.StatusNoContent)
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// isPersonULID reports whether ref is a person ULID rather than a handle.
func isPersonULID(ref string) bool {
	if len(ref) != 26 {
		return false
	}
	for _, c := range ref {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
			return false
		}
	}
	return true
}
