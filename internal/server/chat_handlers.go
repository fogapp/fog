// Chat over HTTP (docs/chat-contract.md): rooms, the ciphertext log,
// invites, group info, blobs, read marks. Every route is person-authed; the
// ones that speak MLS need a device-bound session, because the hub must know
// which leaf is talking.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now()
	}
	return t
}

// chatActor resolves the request's person and device. Routes that need a
// device get a 428 when the session has none — the client must register one
// before it can be a leaf.
func (s *Server) chatActor(w http.ResponseWriter, r *http.Request, needDevice bool) *chat.Actor {
	pc := s.currentPerson(r)
	var dev *store.PersonDevice
	if pc.Session != nil && pc.Session.DeviceID != 0 {
		if d, err := s.store.PersonDeviceByID(pc.Session.DeviceID); err == nil && d.RevokedAt == "" {
			dev = d
		}
	}
	if needDevice && dev == nil {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": chat.ErrNeedDevice.Error(), "code": "need_device"})
		return nil
	}
	a, err := s.chat.ActorFor(pc.Person, dev)
	if err != nil {
		writeError(w, err)
		return nil
	}
	return a
}

// chatError maps service errors onto statuses the client acts on.
func (s *Server) chatError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	tag := ""
	var remote *chat.RemoteError
	switch {
	case errors.As(err, &remote) && remote.Status >= 400 && remote.Status < 600:
		// The hub refused with a code this side has no mapping for: pass
		// its answer through rather than call it our own failure.
		code, tag = remote.Status, remote.Code
	case errors.Is(err, store.ErrNotFound):
		code, tag = http.StatusNotFound, "not_found"
	case errors.Is(err, chat.ErrEpochStale):
		code, tag = http.StatusConflict, "epoch_stale"
	case errors.Is(err, chat.ErrNotParticipant), errors.Is(err, chat.ErrForbidden), errors.Is(err, chat.ErrNotYourLeaf), errors.Is(err, chat.ErrJoinRefused), errors.Is(err, chat.ErrFixedMembership), errors.Is(err, chat.ErrUnverifiedDevice):
		code, tag = http.StatusForbidden, "forbidden"
	case errors.Is(err, chat.ErrBadInput), errors.Is(err, chat.ErrUnsupported), errors.Is(err, chat.ErrGroupMismatch), errors.Is(err, chat.ErrUnknownDevice), errors.Is(err, chat.ErrUnknownPerson):
		code, tag = http.StatusBadRequest, "bad_request"
	case errors.Is(err, chat.ErrNeedDevice):
		code, tag = http.StatusPreconditionRequired, "need_device"
	case errors.Is(err, chat.ErrArchived):
		code, tag = http.StatusGone, "archived"
	case errors.Is(err, chat.ErrRoomMoving):
		code, tag = http.StatusServiceUnavailable, "moving"
	case errors.Is(err, chat.ErrAwaitingRemoval):
		code, tag = http.StatusConflict, "awaiting_removal"
	case errors.Is(err, chat.ErrNeedGroupInfo):
		code, tag = http.StatusConflict, "need_group_info"
	case errors.Is(err, chat.ErrQuota):
		code, tag = http.StatusInsufficientStorage, "quota"
	}
	if code == http.StatusInternalServerError {
		s.log.Error("chat", "err", err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error(), "code": tag})
}

// writeEpochStale answers a submit the room has moved past with where it
// is now, so the client syncs to that epoch and retries. The room is read
// again for the current position; if that read fails the copy the handler
// already holds is the best answer left, never a nil room.
func (s *Server) writeEpochStale(w http.ResponseWriter, room *store.ChatRoom, err error) {
	if fresh, ferr := s.store.ChatRoom(room.ID); ferr == nil {
		room = fresh
	}
	writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "epoch_stale", "epoch": room.Epoch, "seq": room.Seq})
}

func (s *Server) chatRoomFromPath(w http.ResponseWriter, r *http.Request) *store.ChatRoom {
	room, err := s.store.ChatRoom(r.PathValue("id"))
	if err != nil {
		s.chatError(w, err)
		return nil
	}
	return room
}

// displayFor names a person address for the UI: local people by their
// display name, remote ones (Phase 2) by handle and household.
func (s *Server) displayFor(addr string) (string, string) {
	return s.newNamer().name(addr)
}

// namer is displayFor for one response that names many people: it reads
// our identity once, each household's people list once, and each address
// once, however often they come up across rooms and participants.
type namer struct {
	s     *Server
	me    string
	peers map[string]*store.FedPeer // by instance id; nil: no such peer
	memo  map[string][2]string
}

func (s *Server) newNamer() *namer {
	n := &namer{s: s, peers: map[string]*store.FedPeer{}, memo: map[string][2]string{}}
	if me, err := s.fed.Identity(); err == nil {
		n.me = me.InstanceID
	}
	return n
}

func (n *namer) name(addr string) (string, string) {
	if v, ok := n.memo[addr]; ok {
		return v[0], v[1]
	}
	name, handle := n.resolve(addr)
	n.memo[addr] = [2]string{name, handle}
	return name, handle
}

func (n *namer) resolve(addr string) (string, string) {
	inst, handle, ok := chat.ParseAddr(addr)
	if !ok {
		return addr, ""
	}
	if n.me != "" && n.me == inst {
		if p, err := n.s.store.PersonByRef(handle); err == nil {
			return p.DisplayName, handle
		}
	}
	peer, seen := n.peers[inst]
	if !seen {
		peer, _ = n.s.store.FedPeerByInstanceID(inst)
		n.peers[inst] = peer
	}
	if peer != nil {
		for _, dp := range chat.PeerPeople(peer) {
			if dp.Handle == handle {
				return dp.DisplayName + " · " + peer.DisplayName(), handle
			}
		}
		return handle + " · " + peer.DisplayName(), handle
	}
	return handle, handle
}

type roomView struct {
	store.ChatRoom
	Policy       chat.Policy `json:"policy"`
	Participants int         `json:"participants"`
	MyRole       int         `json:"myRole"`
	ReadSeq      int64       `json:"readSeq"`
	Unread       int64       `json:"unread"`
	HubHere      bool        `json:"hubHere"`
	Notify       string      `json:"notify"`
	DM           []string    `json:"dm,omitempty"`
	DMNames      []string    `json:"dmNames,omitempty"`
	Households   int         `json:"households"`
	// Roles is each current participant's role index, by address: what a
	// client checks a moderation act (a delete of someone else's message, a
	// pin) against.
	Roles map[string]int `json:"roles"`
}

// roomView is a room as its participant sees it, from its participant
// rows. names is shared across the rooms of one listing, where the same
// people come up room after room.
func (s *Server) roomView(room *store.ChatRoom, actor *chat.Actor, readMarks map[string]int64, names *namer, parts []store.ChatParticipant) roomView {
	v := roomView{ChatRoom: *room, HubHere: room.HubInstance == "", Roles: map[string]int{}}
	v.Policy, _ = chat.ParsePolicy(room.Policy)
	homes := map[string]bool{}
	for _, p := range parts {
		if p.LeftSeq != 0 {
			continue
		}
		v.Roles[p.PersonAddr] = p.RoleIndex
		v.Participants++
		homes[p.HomeInstance] = true
		if p.PersonAddr == actor.Addr {
			v.MyRole = p.RoleIndex
			v.Notify = p.Notify
		}
	}
	v.Households = len(homes)
	v.ReadSeq = readMarks[room.ID]
	if room.Seq > v.ReadSeq {
		v.Unread = room.Seq - v.ReadSeq
	}
	if room.DMKey != "" {
		for _, a := range strings.Split(room.DMKey, ",") {
			if a == actor.Addr {
				continue
			}
			v.DM = append(v.DM, a)
			name, _ := names.name(a)
			v.DMNames = append(v.DMNames, name)
		}
	}
	return v
}

// singleRoomView is roomView for a response about one room.
func (s *Server) singleRoomView(room *store.ChatRoom, actor *chat.Actor, marks map[string]int64) roomView {
	parts, _ := s.store.ChatParticipants(room.ID)
	return s.roomView(room, actor, marks, s.newNamer(), parts)
}

func (s *Server) handleChatRooms(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	rooms, err := s.store.ChatRoomsFor(actor.Addr)
	if err != nil {
		writeError(w, err)
		return
	}
	marks, _ := s.store.ChatReadMarks(actor.Person.ID)
	ids := make([]string, len(rooms))
	for i := range rooms {
		ids[i] = rooms[i].ID
	}
	parts, _ := s.store.ChatParticipantsIn(ids)
	views := make([]roomView, 0, len(rooms))
	names := s.newNamer()
	for i := range rooms {
		views = append(views, s.roomView(&rooms[i], actor, marks, names, parts[rooms[i].ID]))
	}
	avail, _ := s.store.ChatVisibleChannels(actor.Addr)
	availViews := make([]map[string]any, 0, len(avail))
	for _, a := range avail {
		availViews = append(availViews, map[string]any{
			"id": a.ID, "label": a.Label, "topicLabel": a.TopicLabel, "kind": a.Kind,
			"joinable": a.GroupInfoEpoch == a.Epoch && a.GroupInfoEpoch >= 0,
		})
	}
	invites, _ := s.store.ChatInvitesFor(actor.Addr)
	pending := []map[string]any{}
	for _, inv := range invites {
		if actor.Device == nil || inv.DeviceKey == "" || inv.DeviceKey == people.EncodeKey(actor.Device.DeviceKey) {
			pending = append(pending, map[string]any{"id": inv.ID, "roomId": inv.RoomID, "invitedBy": inv.InvitedBy, "seq": inv.Seq})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rooms": views, "available": availViews, "invites": pending,
		"me": map[string]any{"addr": actor.Addr, "handle": actor.Person.Handle, "displayName": actor.Person.DisplayName},
	})
}

func (s *Server) handleChatCreateRoom(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, true)
	if actor == nil {
		return
	}
	var in chat.CreateRoomInput
	if !readJSON(w, r, &in) {
		return
	}
	room, err := s.chat.CreateRoom(actor, in)
	if err != nil {
		s.chatError(w, err)
		return
	}
	marks, _ := s.store.ChatReadMarks(actor.Person.ID)
	writeJSON(w, http.StatusOK, map[string]any{"room": s.singleRoomView(room, actor, marks)})
}

type participantView struct {
	Addr         string `json:"addr"`
	Handle       string `json:"handle"`
	DisplayName  string `json:"displayName"`
	HomeInstance string `json:"homeInstance"`
	RoleIndex    int    `json:"roleIndex"`
	JoinedSeq    int64  `json:"joinedSeq"`
	LeftSeq      int64  `json:"leftSeq,omitempty"`
	Online       bool   `json:"online"`
}

func (s *Server) handleChatRoom(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	// Non-participants — former and banned ones included: what the room is
	// now is not theirs — may see a joinable channel's summary, nothing
	// more: no one's name, role or device.
	if _, err := s.chat.Participant(room, actor.Addr); err != nil {
		if !(room.Kind == chat.KindChannel && room.WorkspaceVisible && room.HubInstance == "") {
			s.chatError(w, chat.ErrNotParticipant)
			return
		}
		parts, _ := s.store.ChatParticipants(room.ID)
		v := s.roomView(room, actor, nil, s.newNamer(), parts)
		v.Roles = map[string]int{}
		v.MyRole, v.Notify = 0, ""
		writeJSON(w, http.StatusOK, map[string]any{
			"room":           v,
			"participants":   []participantView{},
			"leaves":         []map[string]any{},
			"groupInfoEpoch": room.GroupInfoEpoch,
			"impact":         s.impactSentence(room, nil),
		})
		return
	}
	parts, _ := s.store.ChatParticipants(room.ID)
	names := s.newNamer()
	pv := make([]participantView, 0, len(parts))
	for _, p := range parts {
		name, handle := names.name(p.PersonAddr)
		pv = append(pv, participantView{Addr: p.PersonAddr, Handle: handle, DisplayName: name, HomeInstance: p.HomeInstance,
			RoleIndex: p.RoleIndex, JoinedSeq: p.JoinedSeq, LeftSeq: p.LeftSeq, Online: s.chat.Bus.Online(p.PersonAddr)})
	}
	tree, _ := s.chat.Tree(room)
	leaves := []map[string]any{}
	if tree != nil {
		for _, l := range tree.Members() {
			leaves = append(leaves, map[string]any{"index": l.Index, "identity": l.Identity, "signatureKey": people.EncodeKey(l.SignatureKey)})
		}
	}
	marks, _ := s.store.ChatReadMarks(actor.Person.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"room":           s.roomView(room, actor, marks, names, parts),
		"participants":   pv,
		"leaves":         leaves,
		"groupInfoEpoch": room.GroupInfoEpoch,
		"impact":         s.impactSentence(room, pv),
	})
}

// impactSentence states what the room is in one server-generated line.
func (s *Server) impactSentence(room *store.ChatRoom, parts []participantView) string {
	n, homes := 0, map[string]bool{}
	for _, p := range parts {
		if p.LeftSeq == 0 {
			n++
			homes[p.HomeInstance] = true
		}
	}
	host := s.householdName()
	if room.HubInstance != "" {
		if peer, err := s.store.FedPeerByInstanceID(room.HubInstance); err == nil {
			host = peer.DisplayName()
		}
	}
	people := fmt.Sprintf("%d people", n)
	if n == 1 {
		people = "1 person"
	}
	hh := ""
	if len(homes) > 1 {
		hh = fmt.Sprintf(", from %d households", len(homes))
	}
	return fmt.Sprintf("This room is hosted by %s. The %s in it%s can read every message you send here; fog carries the messages but cannot read them. Your devices keep your copy. Leaving keeps what you have and stops what comes next.", host, people, hh)
}

func (s *Server) handleChatUpdateRoom(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	var req struct {
		chat.RoomPatch
		Notify *string `json:"notify"`
		// AcceptHub: this person agrees to this fog taking the room over
		// from its hub (chat.Federation.AcceptMove). Local, never relayed.
		AcceptHub bool `json:"acceptHub"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.AcceptHub {
		if err := s.chat.Fed.AcceptMove(actor, room); err != nil {
			s.chatError(w, err)
			return
		}
	}
	if req.Notify != nil {
		switch *req.Notify {
		case "all", "mentions", "none":
			if _, err := s.chat.Participant(room, actor.Addr); err != nil {
				s.chatError(w, err)
				return
			}
			s.store.SetChatParticipantNotify(room.ID, actor.Addr, *req.Notify)
		default:
			badRequest(w, "notify must be all, mentions, or none")
			return
		}
	}
	if !req.RoomPatch.Empty() {
		var err error
		switch {
		case room.HubInstance == "":
			err = s.chat.UpdateRoom(actor, room, req.RoomPatch)
		case room.ReplicaStatus != "live":
			err = chat.ErrArchived
		default:
			// The hub is the one authority: a replica's own write would
			// show here alone, and the next snapshot would undo it.
			err = s.chat.Fed.RelayUpdate(r.Context(), actor, room, req.RoomPatch)
		}
		if err != nil {
			s.chatError(w, err)
			return
		}
	}
	room, _ = s.store.ChatRoom(room.ID)
	marks, _ := s.store.ChatReadMarks(actor.Person.ID)
	writeJSON(w, http.StatusOK, map[string]any{"room": s.singleRoomView(room, actor, marks)})
}

func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	evs, err := s.chat.Events(actor, room, after, before, limit)
	if err != nil {
		s.chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": eventViews(evs), "seq": room.Seq, "epoch": room.Epoch})
}

type eventView struct {
	store.ChatEvent
	Hints json.RawMessage `json:"hints,omitempty" ts:"{ mentions?: string[] }"`
}

func eventViews(evs []store.ChatEvent) []eventView {
	out := make([]eventView, 0, len(evs))
	for _, e := range evs {
		out = append(out, eventView1(e))
	}
	return out
}

func eventView1(e store.ChatEvent) eventView {
	v := eventView{ChatEvent: e}
	if e.Hints != "" {
		v.Hints = json.RawMessage(e.Hints)
	}
	return v
}

func (s *Server) handleChatSubmit(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, true)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	var in chat.SubmitInput
	if !readJSON(w, r, &in) {
		return
	}
	var res *chat.Result
	var err error
	if room.HubInstance != "" {
		if room.ReplicaStatus != "live" {
			s.chatError(w, chat.ErrArchived)
			return
		}
		res, err = s.chat.Fed.RelaySubmit(r.Context(), actor, room, in)
	} else {
		res, err = s.chat.Submit(actor, room, in)
	}
	if err != nil {
		if errors.Is(err, chat.ErrEpochStale) {
			s.writeEpochStale(w, room, err)
			return
		}
		s.chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": eventView1(*res.Event), "replayed": res.Replayed})
}

func (s *Server) handleChatGroupInfo(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	var info []byte
	var epoch int64
	var err error
	if room.HubInstance != "" {
		info, epoch, err = s.chat.Fed.FetchGroupInfo(r.Context(), actor, room)
	} else {
		info, epoch, err = s.chat.GroupInfoFor(actor, room)
	}
	if err != nil {
		s.chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groupInfo": people.EncodeKey(info), "epoch": epoch})
}

func (s *Server) handleChatPutGroupInfo(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, true)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	var req struct {
		Epoch     int64  `json:"epoch"`
		GroupInfo string `json:"groupInfo"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	info, err := people.DecodeKey(req.GroupInfo)
	if err != nil {
		badRequest(w, "malformed group info")
		return
	}
	if room.HubInstance != "" {
		err = s.chat.Fed.RelayGroupInfo(r.Context(), actor, room, req.Epoch, info)
	} else {
		err = s.chat.SetGroupInfo(actor, room, req.Epoch, info)
	}
	if err != nil {
		s.chatError(w, err)
		return
	}
	writeOK(w)
}

func (s *Server) handleChatRead(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room := s.chatRoomFromPath(w, r)
	if room == nil {
		return
	}
	var req struct {
		Seq int64 `json:"seq"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := s.store.ChatParticipant(room.ID, actor.Addr); err != nil {
		s.chatError(w, chat.ErrNotParticipant)
		return
	}
	if req.Seq > room.Seq {
		req.Seq = room.Seq
	}
	if err := s.store.SetChatReadMark(actor.Person.ID, room.ID, req.Seq); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

func (s *Server) handleChatTyping(w http.ResponseWriter, r *http.Request) {
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
	s.chat.Bus.PublishRoom(room.ID, chat.Notification{Kind: "ephemeral", RoomID: room.ID, Person: actor.Addr, Signal: "typing"}, nil)
	go s.chat.Fed.SendEphemeral(room, actor.Addr, "typing")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChatInvites(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	invites, err := s.store.ChatInvitesFor(actor.Addr)
	if err != nil {
		writeError(w, err)
		return
	}
	ids := make([]string, len(invites))
	for i, inv := range invites {
		ids[i] = inv.RoomID
	}
	rooms, err := s.store.ChatRoomsByID(ids)
	if err != nil {
		writeError(w, err)
		return
	}
	out := []map[string]any{}
	for _, inv := range invites {
		if actor.Device != nil && inv.DeviceKey != "" && inv.DeviceKey != people.EncodeKey(actor.Device.DeviceKey) {
			continue
		}
		room, ok := rooms[inv.RoomID]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"id": inv.ID, "roomId": inv.RoomID, "invitedBy": inv.InvitedBy, "seq": inv.Seq,
			"welcome": people.EncodeKey(inv.Welcome), "createdAt": inv.CreatedAt,
			"room": map[string]any{"id": room.ID, "kind": room.Kind, "label": room.Label, "epoch": room.Epoch, "seq": room.Seq},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

func (s *Server) handleChatAcceptInvite(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := s.store.AcceptChatInvite(id, actor.Addr); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

func (s *Server) handleChatPutBlob(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	room, err := s.store.ChatRoom(r.URL.Query().Get("room"))
	if err != nil {
		s.chatError(w, err)
		return
	}
	var b *store.ChatBlob
	if room.HubInstance != "" {
		b, err = s.chat.Fed.RelayBlob(r.Context(), actor, room, http.MaxBytesReader(w, r.Body, chat.MaxBlob+1), r.ContentLength)
	} else {
		b, err = s.chat.PutBlob(actor, room, http.MaxBytesReader(w, r.Body, chat.MaxBlob+1), r.ContentLength)
	}
	if err != nil {
		s.chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha256": b.SHA256, "size": b.Size})
}

func (s *Server) handleChatGetBlob(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	f, b, err := s.chat.OpenBlob(actor, r.PathValue("sha"))
	if err != nil && errors.Is(err, store.ErrNotFound) {
		// A replica may not hold the bytes yet: fetch from the hub once.
		if room := s.replicaForBlob(r.URL.Query().Get("room"), actor); room != nil {
			if ferr := s.chat.Fed.FetchBlob(r.Context(), room, r.PathValue("sha")); ferr == nil {
				f, b, err = s.chat.OpenBlob(actor, r.PathValue("sha"))
			}
		}
	}
	if err != nil {
		s.chatError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.FormatInt(b.Size, 10))
	http.ServeContent(w, r, b.SHA256, mustTime(b.StoredAt), f)
}

// replicaForBlob resolves the room a blob fetch names, when it is a replica
// this person is in.
func (s *Server) replicaForBlob(roomID string, actor *chat.Actor) *store.ChatRoom {
	if roomID == "" {
		return nil
	}
	room, err := s.store.ChatRoom(roomID)
	if err != nil || room.HubInstance == "" {
		return nil
	}
	if _, err := s.store.ChatParticipant(room.ID, actor.Addr); err != nil {
		return nil
	}
	return room
}

// handleChatKeysByAddr is key material by address: local people directly,
// a peer's people through their home fog.
func (s *Server) handleChatKeysByAddr(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	addr := r.URL.Query().Get("addr")
	inst, handle, ok := chat.ParseAddr(addr)
	if !ok {
		badRequest(w, "addr must be fog:<instance>/<handle>")
		return
	}
	if s.chat.IsLocal(addr) {
		p, err := s.store.PersonByRef(handle)
		if err != nil || p.Status != store.PersonStatusActive {
			notFound(w, "no such person")
			return
		}
		dev, _ := strconv.ParseInt(r.URL.Query().Get("device"), 10, 64)
		writeJSON(w, http.StatusOK, s.keyMaterialFor(p, DefaultCiphersuite, dev, s.mayConsumeKeys(actor.Person, p)))
		return
	}
	if _, err := s.store.FedPeerByInstanceID(inst); err != nil {
		notFound(w, "that household is not paired with this one")
		return
	}
	raw, err := s.chat.Fed.KeysFor(r.Context(), addr)
	if err != nil {
		s.chatError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}
