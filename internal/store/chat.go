// Chat storage: rooms, participants, the ciphertext log, invites, blobs,
// read marks. Rules live in internal/chat; these are the queries.
package store

import (
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"
)

var ErrDuplicateEvent = errors.New("event id already in this room")

// ErrChatRoomFrozen: the room is frozen for a hub hand-over.
var ErrChatRoomFrozen = errors.New("room frozen for a hand-over")

type ChatRoom struct {
	ID               string `json:"id"`
	GroupID          []byte `json:"-"`
	HubInstance      string `json:"hubInstance"`
	Kind             string `json:"kind" ts:"\"channel\" | \"group\" | \"dm\""`
	Label            string `json:"label"`
	TopicLabel       string `json:"topicLabel"`
	Policy           string `json:"-"`
	WorkspaceVisible bool   `json:"workspaceVisible"`
	Epoch            int64  `json:"epoch"`
	Seq              int64  `json:"seq"`
	Tree             string `json:"-"`
	GroupInfoEpoch   int64  `json:"-"`
	DMKey            string `json:"-"`
	ReplicaStatus    string `json:"replicaStatus"`
	CreatedBy        string `json:"createdBy"`
	CreatedAt        string `json:"createdAt"`
	ArchivedAt       string `json:"archivedAt,omitempty"`
	// PublicState is the hub's verified public state of the MLS group
	// (mlsview.PublicGroup, JSON); '' until seeded.
	PublicState string `json:"-"`
}

const chatRoomCols = `id, group_id, hub_instance, kind, label, topic_label, policy, workspace_visible, epoch, seq, tree, group_info_epoch, dm_key, replica_status, created_by, created_at, COALESCE(archived_at, ''), public_state`

func scanChatRoom(sc scanner) (ChatRoom, error) {
	var r ChatRoom
	var vis int
	err := sc.Scan(&r.ID, &r.GroupID, &r.HubInstance, &r.Kind, &r.Label, &r.TopicLabel, &r.Policy, &vis, &r.Epoch, &r.Seq, &r.Tree, &r.GroupInfoEpoch, &r.DMKey, &r.ReplicaStatus, &r.CreatedBy, &r.CreatedAt, &r.ArchivedAt, &r.PublicState)
	r.WorkspaceVisible = vis == 1
	return r, err
}

func (s *Store) CreateChatRoom(r *ChatRoom) error {
	vis := 0
	if r.WorkspaceVisible {
		vis = 1
	}
	_, err := s.db.Exec(`INSERT INTO chat_rooms (id, group_id, hub_instance, kind, label, topic_label, policy, workspace_visible, epoch, seq, tree, dm_key, replica_status, created_by, created_at, public_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.GroupID, r.HubInstance, r.Kind, r.Label, r.TopicLabel, r.Policy, vis, r.Epoch, r.Seq, r.Tree, r.DMKey, r.ReplicaStatus, r.CreatedBy, now(), r.PublicState)
	return err
}

func (s *Store) ChatRoom(id string) (*ChatRoom, error) {
	return ptr(queryOne(s.rdb, scanChatRoom, `SELECT `+chatRoomCols+` FROM chat_rooms WHERE id = ?`, id))
}

// ChatRoomsByID returns the rooms among ids that exist, keyed by id.
func (s *Store) ChatRoomsByID(ids []string) (map[string]*ChatRoom, error) {
	out := map[string]*ChatRoom{}
	for chunk := range slices.Chunk(ids, inChunk) {
		in, args := inList(chunk)
		rooms, err := queryAll(s.db, scanChatRoom, `SELECT `+chatRoomCols+` FROM chat_rooms WHERE id IN (`+in+`)`, args...)
		if err != nil {
			return nil, err
		}
		for i := range rooms {
			out[rooms[i].ID] = &rooms[i]
		}
	}
	return out, nil
}

func (s *Store) ChatRoomByGroupID(gid []byte) (*ChatRoom, error) {
	return ptr(queryOne(s.db, scanChatRoom, `SELECT `+chatRoomCols+` FROM chat_rooms WHERE group_id = ?`, gid))
}

// ChatRoomByDMKey is the DM hubbed here with that set.
func (s *Store) ChatRoomByDMKey(key string) (*ChatRoom, error) {
	return ptr(queryOne(s.db, scanChatRoom, `SELECT `+chatRoomCols+` FROM chat_rooms WHERE dm_key = ? AND hub_instance = ''`, key))
}

// SetChatRoomDMKey records a replica's DM set, which its hub fixed.
func (s *Store) SetChatRoomDMKey(id, key string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET dm_key = ? WHERE id = ? AND dm_key = ''`, key, id)
	return err
}

// SetChatPublicState stores a room's verified public group state.
func (s *Store) SetChatPublicState(id, state string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET public_state = ? WHERE id = ?`, state, id)
	return err
}

// ChatRoomsFor lists the rooms a person currently participates in.
func (s *Store) ChatRoomsFor(personAddr string) ([]ChatRoom, error) {
	return queryAll(s.rdb, scanChatRoom, `SELECT `+chatRoomCols+` FROM chat_rooms r
		WHERE r.id IN (SELECT room_id FROM chat_participants WHERE person_addr = ? AND left_seq IS NULL)
		ORDER BY r.created_at`, personAddr)
}

// ChatVisibleChannels lists channels the household can see and join, hubbed
// here, that the person is not in.
func (s *Store) ChatVisibleChannels(personAddr string) ([]ChatRoom, error) {
	return queryAll(s.db, scanChatRoom, `SELECT `+chatRoomCols+` FROM chat_rooms r
		WHERE r.kind = 'channel' AND r.workspace_visible = 1 AND r.hub_instance = '' AND r.archived_at IS NULL
		AND r.id NOT IN (SELECT room_id FROM chat_participants WHERE person_addr = ? AND left_seq IS NULL)
		ORDER BY r.label`, personAddr)
}

func (s *Store) AllChatRooms() ([]ChatRoom, error) {
	return queryAll(s.db, scanChatRoom, `SELECT `+chatRoomCols+` FROM chat_rooms ORDER BY created_at`)
}

// SetChatRoomPolicy replaces a room's policy JSON.
func (s *Store) SetChatRoomPolicy(id, policy string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET policy = ? WHERE id = ?`, policy, id)
	return err
}

func (s *Store) UpdateChatRoomLabels(id, label, topic string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET label = ?, topic_label = ? WHERE id = ?`, label, topic, id)
	return err
}

// SetChatRoomWorkspaceVisible sets whether a channel is listed for
// everyone in the household to find and join.
func (s *Store) SetChatRoomWorkspaceVisible(id string, visible bool) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET workspace_visible = ? WHERE id = ?`, boolToInt(visible), id)
	return err
}

// SetChatRoomReplicaStatus sets a replica's status (live, archived, …)
// without touching its hub.
func (s *Store) SetChatRoomReplicaStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET replica_status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) ArchiveChatRoom(id string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET archived_at = ? WHERE id = ? AND archived_at IS NULL`, now(), id)
	return err
}

func (s *Store) SetChatGroupInfo(id string, epoch int64, info []byte) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET group_info = ?, group_info_epoch = ? WHERE id = ?`, info, epoch, id)
	return err
}

func (s *Store) ChatGroupInfo(id string) ([]byte, int64, error) {
	var info []byte
	var epoch int64
	err := s.db.QueryRow(`SELECT group_info, group_info_epoch FROM chat_rooms WHERE id = ?`, id).Scan(&info, &epoch)
	if err == sql.ErrNoRows {
		return nil, 0, ErrNotFound
	}
	return info, epoch, err
}

// ChatRoomHubETag is the ETag of the hub snapshot a replica last applied,
// and the hub's seq that snapshot named (0: not known).
func (s *Store) ChatRoomHubETag(id string) (etag string, seq int64, err error) {
	err = s.db.QueryRow(`SELECT hub_etag, hub_etag_seq FROM chat_rooms WHERE id = ?`, id).Scan(&etag, &seq)
	if err == sql.ErrNoRows {
		return "", 0, ErrNotFound
	}
	return etag, seq, err
}

func (s *Store) SetChatRoomHubETag(id, etag string, seq int64) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET hub_etag = ?, hub_etag_seq = ? WHERE id = ?`, etag, seq, id)
	return err
}

// ---- participants ----

type ChatParticipant struct {
	RoomID       string `json:"-"`
	PersonAddr   string `json:"addr"`
	HomeInstance string `json:"homeInstance"`
	RoleIndex    int    `json:"roleIndex"`
	JoinedSeq    int64  `json:"joinedSeq"`
	LeftSeq      int64  `json:"leftSeq,omitempty"`
	Notify       string `json:"notify"`
	// IdentityKey is the identity key (base64url) the person held when they
	// joined; '' when not known.
	IdentityKey string `json:"identityKey,omitempty"`
}

func (s *Store) ChatParticipants(roomID string) ([]ChatParticipant, error) {
	return queryAll(s.rdb, scanChatParticipant, `SELECT `+chatParticipantCols+` FROM chat_participants WHERE room_id = ? ORDER BY joined_seq`, roomID)
}

// ChatParticipantsIn returns the participant rows of several rooms at
// once, keyed by room id.
func (s *Store) ChatParticipantsIn(roomIDs []string) (map[string][]ChatParticipant, error) {
	out := map[string][]ChatParticipant{}
	for chunk := range slices.Chunk(roomIDs, inChunk) {
		in, args := inList(chunk)
		rows, err := queryAll(s.rdb, scanChatParticipant, `SELECT `+chatParticipantCols+` FROM chat_participants
			WHERE room_id IN (`+in+`) ORDER BY room_id, joined_seq`, args...)
		if err != nil {
			return nil, err
		}
		for _, p := range rows {
			out[p.RoomID] = append(out[p.RoomID], p)
		}
	}
	return out, nil
}

const chatParticipantCols = `room_id, person_addr, home_instance, role_index, joined_seq, COALESCE(left_seq, 0), notify, identity_key`

func scanChatParticipant(sc scanner) (ChatParticipant, error) {
	var p ChatParticipant
	err := sc.Scan(&p.RoomID, &p.PersonAddr, &p.HomeInstance, &p.RoleIndex, &p.JoinedSeq, &p.LeftSeq, &p.Notify, &p.IdentityKey)
	return p, err
}

func (s *Store) ChatParticipant(roomID, addr string) (*ChatParticipant, error) {
	return ptr(queryOne(s.db, scanChatParticipant, `SELECT `+chatParticipantCols+` FROM chat_participants WHERE room_id = ? AND person_addr = ?`, roomID, addr))
}

func (s *Store) SetChatParticipantNotify(roomID, addr, notify string) error {
	_, err := s.db.Exec(`UPDATE chat_participants SET notify = ? WHERE room_id = ? AND person_addr = ?`, notify, roomID, addr)
	return err
}

func (s *Store) SetChatParticipantRole(roomID, addr string, role int) error {
	_, err := s.db.Exec(`UPDATE chat_participants SET role_index = ? WHERE room_id = ? AND person_addr = ?`, role, roomID, addr)
	return err
}

// SetChatParticipantIdentity records the identity key a participant holds,
// for a row from before the hub recorded one.
func (s *Store) SetChatParticipantIdentity(roomID, addr, identityKey string) error {
	_, err := s.db.Exec(`UPDATE chat_participants SET identity_key = ? WHERE room_id = ? AND person_addr = ?`, identityKey, roomID, addr)
	return err
}

// ---- events ----

type ChatEvent struct {
	RoomID       string `json:"roomId"`
	Seq          int64  `json:"seq"`
	ID           string `json:"id"`
	Epoch        int64  `json:"epoch"`
	Kind         string `json:"kind" ts:"\"mls_private\" | \"mls_commit\" | \"mls_proposal\" | \"system\""`
	SenderAddr   string `json:"sender,omitempty"`
	SenderDevice string `json:"senderDevice,omitempty"`
	SentAt       string `json:"sentAt,omitempty"`
	HubAt        string `json:"hubAt"`
	Payload      []byte `json:"payload"`
	Hints        string `json:"-"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	// SenderRole is the sender's role index in the room when the hub
	// sequenced the event (0: not recorded).
	SenderRole int `json:"senderRole,omitempty"`
}

const chatEventCols = `room_id, seq, id, epoch, kind, sender_addr, sender_device, sent_at, hub_at, payload, hints, COALESCE(expires_at, ''), sender_role`

func scanChatEvent(sc scanner) (ChatEvent, error) {
	var e ChatEvent
	err := sc.Scan(&e.RoomID, &e.Seq, &e.ID, &e.Epoch, &e.Kind, &e.SenderAddr, &e.SenderDevice, &e.SentAt, &e.HubAt, &e.Payload, &e.Hints, &e.ExpiresAt, &e.SenderRole)
	return e, err
}

// ChatCommit is the transactional unit of accepting an event at the hub: the
// room's seq (and, for a commit, epoch and tree) move together with the row.
type ChatCommit struct {
	Event *ChatEvent
	// NewEpoch/NewTree are applied when the event is a commit.
	AdvanceEpoch bool
	NewTree      string
	// NewPublicState, when set, replaces the room's verified public state.
	NewPublicState *string
	// Participants to upsert (added) and to mark left, by address.
	Added   []ChatParticipant
	Left    []string
	Invites []ChatInvite
	Policy  *string
}

// AppendChatEvent assigns the next seq to the event and applies the commit's
// side effects atomically. ErrDuplicateEvent when the id was seen before; the
// caller returns the original row.
func (s *Store) AppendChatEvent(c *ChatCommit) error {
	return s.withTx(func(tx *sql.Tx) error {
		e := c.Event
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM chat_events WHERE room_id = ? AND id = ?`, e.RoomID, e.ID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrDuplicateEvent
		}
		var seq int64
		var hub, status string
		if err := tx.QueryRow(`SELECT seq, hub_instance, replica_status FROM chat_rooms WHERE id = ?`, e.RoomID).Scan(&seq, &hub, &status); err != nil {
			return err
		}
		// A room frozen for a hand-over sequences nothing more here, by any
		// path: its final seq is the one the new hub adopts at.
		if hub == "" && status == "frozen" {
			return ErrChatRoomFrozen
		}
		seq++
		e.Seq = seq
		e.HubAt = now()
		if err := insertChatEvent(tx, "INSERT", e.RoomID, e); err != nil {
			return err
		}
		if c.AdvanceEpoch {
			if _, err := tx.Exec(`UPDATE chat_rooms SET seq = ?, epoch = epoch + 1, tree = ? WHERE id = ?`, seq, c.NewTree, e.RoomID); err != nil {
				return err
			}
			if c.NewPublicState != nil {
				if _, err := tx.Exec(`UPDATE chat_rooms SET public_state = ? WHERE id = ?`, *c.NewPublicState, e.RoomID); err != nil {
					return err
				}
			}
		} else {
			if _, err := tx.Exec(`UPDATE chat_rooms SET seq = ? WHERE id = ?`, seq, e.RoomID); err != nil {
				return err
			}
		}
		if c.Policy != nil {
			if _, err := tx.Exec(`UPDATE chat_rooms SET policy = ? WHERE id = ?`, *c.Policy, e.RoomID); err != nil {
				return err
			}
		}
		// A former participant coming back starts again as what Added
		// says (a member), whatever role they held before: a role is held
		// while in the room, not kept for a return.
		for _, p := range c.Added {
			if _, err := tx.Exec(`INSERT INTO chat_participants (room_id, person_addr, home_instance, role_index, joined_seq, left_seq, identity_key) VALUES (?, ?, ?, ?, ?, NULL, ?)
				ON CONFLICT(room_id, person_addr) DO UPDATE SET left_seq = NULL, joined_seq = excluded.joined_seq, home_instance = excluded.home_instance,
					role_index = excluded.role_index, identity_key = excluded.identity_key`,
				e.RoomID, p.PersonAddr, p.HomeInstance, p.RoleIndex, seq, p.IdentityKey); err != nil {
				return err
			}
		}
		if err := markChatParticipantsLeft(tx, e.RoomID, seq, c.Left); err != nil {
			return err
		}
		for _, inv := range c.Invites {
			inv.RoomID, inv.Seq, inv.CreatedAt = e.RoomID, seq, now()
			if err := insertChatInvite(tx, &inv); err != nil {
				return err
			}
		}
		return nil
	})
}

// execer is what both *sql.DB and *sql.Tx offer for writes.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// insertChatEvent writes one event row into roomID's log; verb is "INSERT"
// or "INSERT OR IGNORE".
func insertChatEvent(ex execer, verb, roomID string, e *ChatEvent) error {
	var exp any
	if e.ExpiresAt != "" {
		exp = e.ExpiresAt
	}
	_, err := ex.Exec(verb+` INTO chat_events (room_id, seq, id, epoch, kind, sender_addr, sender_device, sent_at, hub_at, payload, hints, expires_at, sender_role) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		roomID, e.Seq, e.ID, e.Epoch, e.Kind, e.SenderAddr, e.SenderDevice, e.SentAt, e.HubAt, e.Payload, e.Hints, exp, e.SenderRole)
	return err
}

func insertChatInvite(ex execer, inv *ChatInvite) error {
	_, err := ex.Exec(`INSERT INTO chat_invites (room_id, person_addr, device_key, invited_by, seq, welcome, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		inv.RoomID, inv.PersonAddr, inv.DeviceKey, inv.InvitedBy, inv.Seq, inv.Welcome, inv.CreatedAt)
	return err
}

// markChatParticipantsLeft records addrs as having left roomID at seq,
// leaving anyone already out as they were. A Welcome still waiting for them
// in that room goes too: it was their way in while they belonged, and a
// pending invite also lets a device join by itself.
func markChatParticipantsLeft(ex execer, roomID string, seq int64, addrs []string) error {
	for _, addr := range addrs {
		if _, err := ex.Exec(`UPDATE chat_participants SET left_seq = ? WHERE room_id = ? AND person_addr = ? AND left_seq IS NULL`, seq, roomID, addr); err != nil {
			return err
		}
		if _, err := ex.Exec(`DELETE FROM chat_invites WHERE room_id = ? AND person_addr = ? AND accepted_at IS NULL AND seq <= ?`, roomID, addr, seq); err != nil {
			return err
		}
	}
	return nil
}

// MarkChatParticipantLeft records that a person left a room at seq, if they
// were still in it — how a replica learns its people are out when the hub
// no longer shows it the room.
func (s *Store) MarkChatParticipantLeft(roomID, addr string, seq int64) error {
	return markChatParticipantsLeft(s.db, roomID, seq, []string{addr})
}

// ApplyChatReplicaEvent stores an event the hub already sequenced, as-is,
// and moves the replica's seq and epoch to match, in one transaction. A
// repeat of an event already held is ignored.
func (s *Store) ApplyChatReplicaEvent(roomID string, e *ChatEvent, epoch int64) error {
	return s.withTx(func(tx *sql.Tx) error {
		if err := insertChatEvent(tx, "INSERT OR IGNORE", roomID, e); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE chat_rooms SET seq = ?, epoch = ? WHERE id = ?`, e.Seq, epoch, roomID); err != nil {
			return err
		}
		return nil
	})
}

// AddChatBan records a ban on someone who was never in a room: a banned row
// whose left_seq is -1, out from before the start, so every read that lets a
// former participant see up to their departure lets them see nothing.
func (s *Store) AddChatBan(roomID, addr, home string) error {
	_, err := s.db.Exec(`INSERT INTO chat_participants (room_id, person_addr, home_instance, role_index, joined_seq, left_seq, identity_key)
		SELECT id, ?, ?, 1, seq, -1, '' FROM chat_rooms WHERE id = ?
		ON CONFLICT(room_id, person_addr) DO UPDATE SET role_index = 1`, addr, home, roomID)
	return err
}

// SkipChatReplicaSeq moves a replica's seq forward to seq, past events its
// hub no longer holds. It never moves it back.
func (s *Store) SkipChatReplicaSeq(roomID string, seq int64) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET seq = ? WHERE id = ? AND seq < ? AND hub_instance <> ''`, seq, roomID, seq)
	return err
}

// ChatReplicaMeta is the hub's public room state a replica mirrors.
type ChatReplicaMeta struct {
	Label, TopicLabel, Policy, Tree string
	Epoch                           int64
	ArchivedAt                      string
	// PublicState, when set, replaces the replica's copy of the hub's
	// verified group state (what it verifies with if the hub moves here).
	PublicState  string
	Participants []ChatParticipant
	// GroupInfo, when present, replaces the stored group info.
	GroupInfo      []byte
	GroupInfoEpoch int64
}

// ReplaceChatReplicaMeta overwrites a replica's labels, policy, tree and
// participant list with the hub's, in one transaction. The epoch only moves
// forward: events applied since the snapshot was taken may be ahead of it.
// A participant's notify setting is theirs, kept by their home fog, so the
// replica keeps the one it holds rather than take the hub's.
func (s *Store) ReplaceChatReplicaMeta(roomID string, m *ChatReplicaMeta) error {
	return s.withTx(func(tx *sql.Tx) error {
		kept := map[string]string{}
		if err := queryEach(tx, func(sc scanner) error {
			var addr, notify string
			if err := sc.Scan(&addr, &notify); err != nil {
				return err
			}
			kept[addr] = notify
			return nil
		}, `SELECT person_addr, notify FROM chat_participants WHERE room_id = ?`, roomID); err != nil {
			return err
		}
		var archived any
		if m.ArchivedAt != "" {
			archived = m.ArchivedAt
		}
		if _, err := tx.Exec(`UPDATE chat_rooms SET label = ?, topic_label = ?, policy = ?, epoch = MAX(epoch, ?), tree = ?, archived_at = ? WHERE id = ?`,
			m.Label, m.TopicLabel, m.Policy, m.Epoch, m.Tree, archived, roomID); err != nil {
			return err
		}
		if m.PublicState != "" {
			if _, err := tx.Exec(`UPDATE chat_rooms SET public_state = ? WHERE id = ?`, m.PublicState, roomID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM chat_participants WHERE room_id = ?`, roomID); err != nil {
			return err
		}
		for _, p := range m.Participants {
			var left any
			if p.LeftSeq != 0 {
				left = p.LeftSeq
			}
			notify, ok := kept[p.PersonAddr]
			if !ok {
				notify = "all"
			}
			if _, err := tx.Exec(`INSERT INTO chat_participants (room_id, person_addr, home_instance, role_index, joined_seq, left_seq, notify, identity_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				roomID, p.PersonAddr, p.HomeInstance, p.RoleIndex, p.JoinedSeq, left, notify, p.IdentityKey); err != nil {
				return err
			}
			if p.LeftSeq != 0 {
				if _, err := tx.Exec(`DELETE FROM chat_invites WHERE room_id = ? AND person_addr = ? AND accepted_at IS NULL AND seq <= ?`, roomID, p.PersonAddr, p.LeftSeq); err != nil {
					return err
				}
			}
		}
		if len(m.GroupInfo) > 0 {
			if _, err := tx.Exec(`UPDATE chat_rooms SET group_info = ?, group_info_epoch = ? WHERE id = ?`, m.GroupInfo, m.GroupInfoEpoch, roomID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ChatEventByID(roomID, id string) (*ChatEvent, error) {
	return ptr(queryOne(s.rdb, scanChatEvent, `SELECT `+chatEventCols+` FROM chat_events WHERE room_id = ? AND id = ?`, roomID, id))
}

// ChatEventsAfter returns events with seq > after, oldest first, up to limit.
func (s *Store) ChatEventsAfter(roomID string, after int64, limit int) ([]ChatEvent, error) {
	return queryAll(s.rdb, scanChatEvent, `SELECT `+chatEventCols+` FROM chat_events WHERE room_id = ? AND seq > ? ORDER BY seq LIMIT ?`, roomID, after, limit)
}

// ChatEventsBefore returns events with seq < before, newest first, up to limit.
func (s *Store) ChatEventsBefore(roomID string, before int64, limit int) ([]ChatEvent, error) {
	return queryAll(s.rdb, scanChatEvent, `SELECT `+chatEventCols+` FROM chat_events WHERE room_id = ? AND seq < ? ORDER BY seq DESC LIMIT ?`, roomID, before, limit)
}

// PruneChatEvents applies per-room retention. The bot's plaintext of a
// message goes with it — a room that keeps a week of history must not keep
// a readable copy of it forever — and so does any bot copy older than the
// cutoff whose event is already gone.
func (s *Store) PruneChatEvents(roomID string, cutoff string) (int64, error) {
	return s.pruneChatEvents(`room_id = ? AND kind = 'mls_private' AND hub_at < ?`, []any{roomID, cutoff},
		`room_id = ? AND at < ?`, []any{roomID, cutoff})
}

// PruneExpiredChatEvents removes disappearing messages past their expiry,
// with the bot's plaintext of them.
func (s *Store) PruneExpiredChatEvents() (int64, error) {
	return s.pruneChatEvents(`expires_at IS NOT NULL AND expires_at < ?`, []any{now()}, "", nil)
}

// pruneChatEvents deletes the events matching where, pruneBatch at a time
// like pruneBatched, each batch together with the bot's plaintext of those
// events in one transaction. Bot messages matching botWhere (copies whose
// event is already gone) go afterwards; they have no event to stay atomic
// with.
func (s *Store) pruneChatEvents(where string, args []any, botWhere string, botArgs []any) (int64, error) {
	// The two statements of a batch pick the same rows: nothing changes
	// chat_events between them, and the order is fixed.
	batch := `SELECT rowid FROM chat_events WHERE ` + where + ` ORDER BY rowid LIMIT ?`
	batchArgs := append(append([]any{}, args...), pruneBatch)
	var total int64
	for {
		var n int64
		err := s.withTx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM chat_bot_messages WHERE (room_id, seq) IN
				(SELECT room_id, seq FROM chat_events WHERE rowid IN (`+batch+`))`, batchArgs...); err != nil {
				return err
			}
			res, err := tx.Exec(`DELETE FROM chat_events WHERE rowid IN (`+batch+`)`, batchArgs...)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatch {
			break
		}
	}
	if botWhere != "" {
		if _, err := s.pruneBatched("chat_bot_messages", botWhere, botArgs...); err != nil {
			return total, err
		}
	}
	return total, nil
}

// ---- invites ----

type ChatInvite struct {
	ID         int64  `json:"id"`
	RoomID     string `json:"roomId"`
	PersonAddr string `json:"person"`
	DeviceKey  string `json:"deviceKey,omitempty"`
	InvitedBy  string `json:"invitedBy"`
	Seq        int64  `json:"seq"`
	Welcome    []byte `json:"welcome"`
	CreatedAt  string `json:"createdAt"`
	AcceptedAt string `json:"acceptedAt,omitempty"`
}

// ChatInvitesFor lists unaccepted Welcomes addressed to a person, optionally
// narrowed to one device key.
func (s *Store) ChatInvitesFor(personAddr string) ([]ChatInvite, error) {
	return queryAll(s.db, scanChatInvite, `SELECT id, room_id, person_addr, device_key, invited_by, seq, welcome, created_at, COALESCE(accepted_at, '') FROM chat_invites WHERE person_addr = ? AND accepted_at IS NULL ORDER BY id`, personAddr)
}

func scanChatInvite(sc scanner) (ChatInvite, error) {
	var i ChatInvite
	err := sc.Scan(&i.ID, &i.RoomID, &i.PersonAddr, &i.DeviceKey, &i.InvitedBy, &i.Seq, &i.Welcome, &i.CreatedAt, &i.AcceptedAt)
	return i, err
}

// AddChatInvite files a Welcome that arrived from another household's hub.
func (s *Store) AddChatInvite(inv *ChatInvite) error {
	return insertChatInvite(s.db, inv)
}

// ChatRoomInvites lists every invite on file for a room, accepted or not.
func (s *Store) ChatRoomInvites(roomID string) ([]ChatInvite, error) {
	return queryAll(s.db, scanChatInvite, `SELECT id, room_id, person_addr, device_key, invited_by, seq, welcome, created_at, COALESCE(accepted_at, '') FROM chat_invites WHERE room_id = ? ORDER BY id`, roomID)
}

// DeleteChatDecoy drops a replica held only by decoy invites (person
// "decoy:…"), with everything held for it — never one any of this fog's
// people (home) were ever in, or invited to for real.
func (s *Store) DeleteChatDecoy(id, home string) error {
	_, err := s.db.Exec(`DELETE FROM chat_rooms WHERE id = ? AND hub_instance <> ''
		AND NOT EXISTS (SELECT 1 FROM chat_invites WHERE room_id = ? AND person_addr NOT LIKE 'decoy:%')
		AND NOT EXISTS (SELECT 1 FROM chat_participants WHERE room_id = ? AND home_instance = ?)
		AND NOT EXISTS (SELECT 1 FROM chat_read_marks WHERE room_id = ?)`, id, id, id, home, id)
	return err
}

// ChatWatchedRooms counts replicas being watched for unaccepted invites:
// with an invite still pending made since since, and no one who accepted
// one — hubbed by hub, sent by inviter, and in all.
func (s *Store) ChatWatchedRooms(hub, inviter, since string) (byHub, byInviter, total int, err error) {
	err = s.rdb.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN r.hub_instance = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN EXISTS (SELECT 1 FROM chat_invites i WHERE i.room_id = r.id AND i.invited_by = ? AND i.accepted_at IS NULL) THEN 1 ELSE 0 END), 0),
		COUNT(*) FROM chat_rooms r
		WHERE r.hub_instance <> ''
		AND EXISTS (SELECT 1 FROM chat_invites i WHERE i.room_id = r.id AND i.accepted_at IS NULL AND i.created_at >= ?)
		AND NOT EXISTS (SELECT 1 FROM chat_invites i WHERE i.room_id = r.id AND i.accepted_at IS NOT NULL)`, hub, inviter, since).Scan(&byHub, &byInviter, &total)
	return
}

// ChatDecoyOnly says a replica is held only by decoy invites.
func (s *Store) ChatDecoyOnly(id string) bool {
	var real, decoy int
	s.rdb.QueryRow(`SELECT COALESCE(SUM(CASE WHEN person_addr LIKE 'decoy:%' THEN 0 ELSE 1 END), 0), COALESCE(SUM(CASE WHEN person_addr LIKE 'decoy:%' THEN 1 ELSE 0 END), 0) FROM chat_invites WHERE room_id = ?`, id).Scan(&real, &decoy)
	return decoy > 0 && real == 0
}

// ChatPendingInviteAddrs lists the people with an unaccepted invite to a
// room.
func (s *Store) ChatPendingInviteAddrs(roomID string) ([]string, error) {
	return queryAll(s.db, scanValue[string], `SELECT person_addr FROM chat_invites WHERE room_id = ? AND accepted_at IS NULL`, roomID)
}

// ChatInvitedTo says a person has an invite to a room on file, pending or
// accepted.
func (s *Store) ChatInvitedTo(roomID, personAddr string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM chat_invites WHERE room_id = ? AND person_addr = ?`, roomID, personAddr).Scan(&n)
	return n > 0
}

// DropChatInvitesFor deletes the Welcomes still waiting for a person —
// addressed to devices an identity reset just revoked.
func (s *Store) DropChatInvitesFor(personAddr string) error {
	_, err := s.db.Exec(`DELETE FROM chat_invites WHERE person_addr = ? AND accepted_at IS NULL`, personAddr)
	return err
}

func (s *Store) AcceptChatInvite(id int64, personAddr string) error {
	_, err := s.db.Exec(`UPDATE chat_invites SET accepted_at = ? WHERE id = ? AND person_addr = ?`, now(), id, personAddr)
	return err
}

// PruneChatInvites drops invites accepted before cutoff; the membership
// they led to is the record. Each person's latest accepted invite to a room
// stays: it is how a follower knows they accepted, on any device, which
// keeps their room polled and out of the count of watched rooms.
func (s *Store) PruneChatInvites(cutoff string) (int64, error) {
	return s.pruneBatched("chat_invites", `accepted_at IS NOT NULL AND accepted_at < ?
		AND id NOT IN (SELECT MAX(id) FROM chat_invites WHERE accepted_at IS NOT NULL GROUP BY room_id, person_addr)`, cutoff)
}

// ---- blobs ----

type ChatBlob struct {
	SHA256       string `json:"sha256"`
	RoomID       string `json:"roomId"`
	Size         int64  `json:"size"`
	UploaderAddr string `json:"uploader"`
	StoredAt     string `json:"storedAt"`
	// RoomSeq is the room's seq when the blob was stored (0: not known).
	RoomSeq int64 `json:"roomSeq"`
}

// SetChatRoomHub changes who hubs a room and the replica status in one step.
func (s *Store) SetChatRoomHub(id, hub, status string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET hub_instance = ?, replica_status = ? WHERE id = ?`, hub, status, id)
	return err
}

// ChatBlobsForRoom lists the digests of a room's stored attachments.
func (s *Store) ChatBlobsForRoom(roomID string) ([]string, error) {
	return queryAll(s.db, scanValue[string], `SELECT sha256 FROM chat_blobs WHERE room_id = ? ORDER BY stored_at`, roomID)
}

// SetChatRoomTree replaces a room's public leaf list (a leaf marked as its
// device's own departure).
func (s *Store) SetChatRoomTree(id, tree string) error {
	_, err := s.db.Exec(`UPDATE chat_rooms SET tree = ? WHERE id = ?`, tree, id)
	return err
}

// DropDeviceKeyPackages removes every package a device published and no one
// has taken yet, and its last-resort one: what a device restored from a
// backup, which holds none of their private keys, replaces.
func (s *Store) DropDeviceKeyPackages(deviceID int64) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM people_key_packages WHERE device_id = ? AND consumed_at IS NULL`, deviceID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM chat_last_resort_kp WHERE device_id = ?`, deviceID)
		return err
	})
}

// ChatDepartureCap bounds one hub's owed departures. Past it a new one is
// refused (ErrDepartureQueueFull), never an old one dropped: each is a
// person's removal a hub must hear of.
var ChatDepartureCap = 100000

// ErrDepartureQueueFull: a hub owes more departures than the cap.
var ErrDepartureQueueFull = errors.New("too many departures owed to one hub")

// EnqueueChatDeparture queues one departure owed to a hub, refusing it
// rather than dropping any when the hub's queue is full.
func (s *Store) EnqueueChatDeparture(peerID int64, kind, roomID, payload string) error {
	return s.withTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM fed_outbox WHERE peer_id = ? AND kind = ?`, peerID, kind).Scan(&n); err != nil {
			return err
		}
		if n >= ChatDepartureCap {
			return ErrDepartureQueueFull
		}
		t := now()
		_, err := tx.Exec(`INSERT INTO fed_outbox (peer_id, kind, app_name, payload, next_attempt_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`, peerID, kind, roomID, payload, t, t)
		return err
	})
}

// ChatCacheUsage sums the cached copies of blobs of rooms hubbed elsewhere.
func (s *Store) ChatCacheUsage() (int64, error) {
	var n int64
	err := s.rdb.QueryRow(`SELECT COALESCE(SUM(b.size), 0) FROM chat_blobs b JOIN chat_rooms r ON r.id = b.room_id WHERE r.hub_instance <> '' AND b.uploader_addr = 'hub'`).Scan(&n)
	return n, err
}

// OldestCachedChatBlobs lists the oldest cached copies of blobs of rooms
// hubbed elsewhere: what gives way when the cache is full.
func (s *Store) OldestCachedChatBlobs(limit int) ([]ChatBlob, error) {
	return queryAll(s.rdb, func(r scanner) (ChatBlob, error) {
		var b ChatBlob
		return b, r.Scan(&b.SHA256, &b.Size)
	}, `SELECT b.sha256, b.size FROM chat_blobs b JOIN chat_rooms r ON r.id = b.room_id WHERE r.hub_instance <> '' AND b.uploader_addr = 'hub' ORDER BY b.stored_at, b.sha256 LIMIT ?`, limit)
}

// HandOffChatBlobs marks a room's blobs, as it is handed to another hub,
// as ones this fog must keep until that hub has them: none is a cache copy
// (uploader "hub") to evict before the new hub confirms it holds it.
func (s *Store) HandOffChatBlobs(roomID string) error {
	_, err := s.db.Exec(`UPDATE chat_blobs SET uploader_addr = 'handoff' WHERE room_id = ? AND uploader_addr = 'hub'`, roomID)
	return err
}

// ConfirmChatBlobHandedOff makes a handed-off blob a cache copy once the
// new hub has fetched it.
func (s *Store) ConfirmChatBlobHandedOff(sha string) error {
	_, err := s.db.Exec(`UPDATE chat_blobs SET uploader_addr = 'hub' WHERE sha256 = ? AND uploader_addr = 'handoff'`, sha)
	return err
}

// ChatBlobUsage sums stored attachments: a room's, one uploader's, and all.
func (s *Store) ChatBlobUsage(roomID, uploader string) (byRoom, byUploader, total int64, err error) {
	err = s.rdb.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN room_id = ? THEN size END), 0),
		COALESCE(SUM(CASE WHEN uploader_addr = ? THEN size END), 0),
		COALESCE(SUM(size), 0) FROM chat_blobs`, roomID, uploader).Scan(&byRoom, &byUploader, &total)
	return
}

func (s *Store) PutChatBlob(b *ChatBlob) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO chat_blobs (sha256, room_id, size, uploader_addr, stored_at, room_seq) VALUES (?, ?, ?, ?, ?, ?)`,
		b.SHA256, b.RoomID, b.Size, b.UploaderAddr, now(), b.RoomSeq)
	return err
}

func (s *Store) ChatBlob(sha string) (*ChatBlob, error) {
	return ptr(queryOne(s.db, func(sc scanner) (ChatBlob, error) {
		var b ChatBlob
		err := sc.Scan(&b.SHA256, &b.RoomID, &b.Size, &b.UploaderAddr, &b.StoredAt, &b.RoomSeq)
		return b, err
	}, `SELECT sha256, room_id, size, uploader_addr, stored_at, room_seq FROM chat_blobs WHERE sha256 = ?`, sha))
}

func (s *Store) DeleteChatBlob(sha string) error {
	_, err := s.db.Exec(`DELETE FROM chat_blobs WHERE sha256 = ?`, sha)
	return err
}

// ChatBlobsBefore lists a room's blobs stored before the cutoff. Messages are
// sealed, so the hub never learns which one references an attachment; a blob
// is instead as old as the message it was uploaded for, and retention ages
// both out together.
func (s *Store) ChatBlobsBefore(roomID, cutoff string) ([]string, error) {
	return queryAll(s.db, scanValue[string], `SELECT sha256 FROM chat_blobs WHERE room_id = ? AND stored_at < ?`, roomID, cutoff)
}

// ---- read marks ----

func (s *Store) SetChatReadMark(personID int64, roomID string, seq int64) error {
	_, err := s.db.Exec(`INSERT INTO chat_read_marks (person_id, room_id, read_seq, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(person_id, room_id) DO UPDATE SET read_seq = MAX(read_seq, excluded.read_seq), updated_at = excluded.updated_at`,
		personID, roomID, seq, now())
	return err
}

func (s *Store) ChatReadMarks(personID int64) (map[string]int64, error) {
	out := map[string]int64{}
	err := queryEach(s.db, func(sc scanner) error {
		var id string
		var seq int64
		if err := sc.Scan(&id, &seq); err != nil {
			return err
		}
		out[id] = seq
		return nil
	}, `SELECT room_id, read_seq FROM chat_read_marks WHERE person_id = ?`, personID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- bot ----

func (s *Store) ChatBotSeen(kind, key string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM chat_bot_seen WHERE kind = ? AND key = ?`, kind, key).Scan(&n)
	return n > 0, err
}

func (s *Store) MarkChatBotSeen(kind, key string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO chat_bot_seen (kind, key, seen_at) VALUES (?, ?, ?)`, kind, key, time.Now().UTC().Format(time.RFC3339))
	return err
}

// PruneChatBotSeen forgets the bot's "already mirrored" mark for notices
// that are gone from the inbox. Dismissal is final — a notice raised again
// is a new row with a new id — so a dismissed notice's mark can never be
// consulted again, and without this the table grew by one row per notice
// forever.
func (s *Store) PruneChatBotSeen() (int64, error) {
	return s.pruneBatched("chat_bot_seen", `kind = 'notice'
		AND CAST(key AS INTEGER) NOT IN (SELECT id FROM notices WHERE dismissed_at IS NULL)`)
}

// ---- preferences ----

func (s *Store) PersonPrefs(personID int64) (string, error) {
	prefs, err := queryOne(s.db, scanValue[string], `SELECT prefs FROM people WHERE id = ?`, personID)
	if err != nil {
		return "", err
	}
	if prefs == "" {
		prefs = "{}"
	}
	return prefs, nil
}

func (s *Store) SetPersonPrefs(personID int64, prefs string) error {
	_, err := s.db.Exec(`UPDATE people SET prefs = ? WHERE id = ?`, prefs, personID)
	return err
}

// ---- last-resort key packages ----

// SetLastResortKeyPackage files (or replaces) a device's last-resort
// KeyPackage for a ciphersuite.
func (s *Store) SetLastResortKeyPackage(deviceID int64, ciphersuite int, kp []byte) error {
	_, err := s.db.Exec(`INSERT INTO chat_last_resort_kp (device_id, ciphersuite, key_package, published_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(device_id, ciphersuite) DO UPDATE SET key_package = excluded.key_package, published_at = excluded.published_at`,
		deviceID, ciphersuite, kp, now())
	return err
}

// LastResortKeyPackage is a live device's last-resort KeyPackage, if it
// published one; it is never consumed.
func (s *Store) LastResortKeyPackage(deviceID int64, ciphersuite int) ([]byte, error) {
	return queryOne(s.db, scanValue[[]byte], `SELECT k.key_package FROM chat_last_resort_kp k
		JOIN people_devices d ON d.id = k.device_id
		WHERE k.device_id = ? AND k.ciphersuite = ? AND d.revoked_at IS NULL`, deviceID, ciphersuite)
}

// ---- push subscriptions ----

type PushSub struct {
	ID        int64  `json:"id"`
	DeviceID  int64  `json:"deviceId"`
	Endpoint  string `json:"endpoint"`
	P256dh    string `json:"p256dh"`
	Auth      string `json:"auth"`
	CreatedAt string `json:"createdAt"`
	Failures  int    `json:"failures"`
}

func (s *Store) PutPushSub(deviceID int64, endpoint, p256dh, auth string) error {
	_, err := s.db.Exec(`INSERT INTO chat_push_subs (device_id, endpoint, p256dh, auth, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET device_id = excluded.device_id, p256dh = excluded.p256dh, auth = excluded.auth, failures = 0`,
		deviceID, endpoint, p256dh, auth, now())
	return err
}

func (s *Store) DeletePushSubs(deviceID int64) error {
	_, err := s.db.Exec(`DELETE FROM chat_push_subs WHERE device_id = ?`, deviceID)
	return err
}

func (s *Store) DeletePushSub(endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM chat_push_subs WHERE endpoint = ?`, endpoint)
	return err
}

func (s *Store) NotePushFailure(endpoint string) error {
	_, err := s.db.Exec(`UPDATE chat_push_subs SET failures = failures + 1 WHERE endpoint = ?`, endpoint)
	return err
}

// PushSubsForPerson lists the subscriptions of a person's live devices.
func (s *Store) PushSubsForPerson(personID int64) ([]PushSub, error) {
	return queryAll(s.db, func(sc scanner) (PushSub, error) {
		var p PushSub
		err := sc.Scan(&p.ID, &p.DeviceID, &p.Endpoint, &p.P256dh, &p.Auth, &p.CreatedAt, &p.Failures)
		return p, err
	}, `SELECT ps.id, ps.device_id, ps.endpoint, ps.p256dh, ps.auth, ps.created_at, ps.failures
		FROM chat_push_subs ps JOIN people_devices d ON d.id = ps.device_id
		WHERE d.person_id = ? AND d.revoked_at IS NULL`, personID)
}

// NotifyRecipient is what notifying one person takes: their prefs JSON
// ("{}" when unset) and the push subscriptions of their live devices.
type NotifyRecipient struct {
	PersonID int64
	Prefs    string
	Subs     []PushSub
}

// NotifyRecipients is PersonByRef, PersonPrefs and PushSubsForPerson for
// several people at once — one read of the people, one of their
// subscriptions — keyed by the ref asked for. A ref is a
// person's uid or handle, read the way PersonByRef reads it; refs naming
// no one have no entry.
func (s *Store) NotifyRecipients(refs []string) (map[string]NotifyRecipient, error) {
	var uids, handles []string
	for _, r := range refs {
		if isULID(r) {
			uids = append(uids, r)
		} else {
			handles = append(handles, r)
		}
	}
	type person struct {
		id          int64
		handle, uid string
		prefs       string
	}
	var people []person
	scan := func(sc scanner) error {
		var p person
		if err := sc.Scan(&p.id, &p.handle, &p.uid, &p.prefs); err != nil {
			return err
		}
		if p.prefs == "" {
			p.prefs = "{}"
		}
		people = append(people, p)
		return nil
	}
	for _, by := range []struct {
		col  string
		vals []string
	}{{"uid", uids}, {"handle", handles}} {
		for chunk := range slices.Chunk(by.vals, inChunk) {
			in, args := inList(chunk)
			if err := queryEach(s.db, scan, `SELECT id, handle, COALESCE(uid, ''), prefs FROM people
				WHERE `+by.col+` IN (`+in+`)`, args...); err != nil {
				return nil, err
			}
		}
	}

	out := map[string]NotifyRecipient{}
	byID := map[int64]person{}
	for _, p := range people {
		byID[p.id] = p
	}
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	subs := map[int64][]PushSub{}
	for chunk := range slices.Chunk(ids, inChunk) {
		in, args := inList(chunk)
		err := queryEach(s.db, func(sc scanner) error {
			var personID int64
			var p PushSub
			if err := sc.Scan(&personID, &p.ID, &p.DeviceID, &p.Endpoint, &p.P256dh, &p.Auth, &p.CreatedAt, &p.Failures); err != nil {
				return err
			}
			subs[personID] = append(subs[personID], p)
			return nil
		}, `SELECT d.person_id, ps.id, ps.device_id, ps.endpoint, ps.p256dh, ps.auth, ps.created_at, ps.failures
			FROM chat_push_subs ps JOIN people_devices d ON d.id = ps.device_id
			WHERE d.person_id IN (`+in+`) AND d.revoked_at IS NULL`, args...)
		if err != nil {
			return nil, err
		}
	}
	for _, p := range byID {
		r := NotifyRecipient{PersonID: p.id, Prefs: p.prefs, Subs: subs[p.id]}
		if p.uid != "" && slices.Contains(uids, p.uid) {
			out[p.uid] = r
		}
		if slices.Contains(handles, p.handle) {
			out[p.handle] = r
		}
	}
	return out, nil
}

// ---- webhooks ----

type ChatWebhook struct {
	ID         int64  `json:"id"`
	RoomID     string `json:"roomId"`
	Name       string `json:"name"`
	CreatedBy  string `json:"createdBy"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

func (s *Store) CreateChatWebhook(roomID, name, digest, createdBy string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO chat_webhooks (room_id, name, token_digest, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		roomID, name, digest, createdBy, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ChatWebhooks(roomID string) ([]ChatWebhook, error) {
	return queryAll(s.db, scanChatWebhook, `SELECT `+chatWebhookCols+` FROM chat_webhooks WHERE room_id = ? ORDER BY id`, roomID)
}

const chatWebhookCols = `id, room_id, name, created_by, created_at, COALESCE(last_used_at, '')`

func scanChatWebhook(sc scanner) (ChatWebhook, error) {
	var w ChatWebhook
	err := sc.Scan(&w.ID, &w.RoomID, &w.Name, &w.CreatedBy, &w.CreatedAt, &w.LastUsedAt)
	return w, err
}

func (s *Store) ChatWebhookByDigest(digest string) (*ChatWebhook, error) {
	w, err := queryOne(s.db, scanChatWebhook, `SELECT `+chatWebhookCols+` FROM chat_webhooks WHERE token_digest = ?`, digest)
	if err != nil {
		return nil, err
	}
	s.db.Exec(`UPDATE chat_webhooks SET last_used_at = ? WHERE id = ?`, now(), w.ID)
	return &w, nil
}

// DeleteChatWebhooksBy deletes the webhooks a person made in a room.
// ChatIdentityKeysFor lists every identity key an address has held in any
// room here, past rows and tombstones of deleted people included.
func (s *Store) ChatIdentityKeysFor(addr string) ([]string, error) {
	return queryAll(s.rdb, func(r scanner) (string, error) {
		var k string
		return k, r.Scan(&k)
	}, `SELECT DISTINCT identity_key FROM chat_participants WHERE person_addr = ? AND identity_key <> ''`, addr)
}

// DeleteChatWebhooksByPerson revokes every webhook a person made, in any room.
func (s *Store) DeleteChatWebhooksByPerson(createdBy string) error {
	_, err := s.db.Exec(`DELETE FROM chat_webhooks WHERE created_by = ?`, createdBy)
	return err
}

// RetireChatAddr moves every chat row naming addr — participant rows, bans
// and DM sets included, in rooms hubbed here and replicas alike — to
// retired, the deleted person's address that no living person answers to.
func (s *Store) RetireChatAddr(addr, retired string) error {
	return s.withTx(func(tx *sql.Tx) error {
		// A tombstone stays under the address in each room, out from before
		// the start (left_seq -1) and giving nothing, with the identity key
		// they held: so the address's history of keys outlives them, and a
		// new person given it can't walk into an open channel whose members
		// pinned the old one (ChatIdentityKeysFor).
		if _, err := tx.Exec(`INSERT INTO chat_participants (room_id, person_addr, home_instance, role_index, joined_seq, left_seq, identity_key)
			SELECT room_id, ?, home_instance, 2, joined_seq, -1, identity_key FROM chat_participants WHERE person_addr = ? AND identity_key <> ''`, retired+"\x00tombstone", addr); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE chat_participants SET person_addr = ? WHERE person_addr = ?`,
			`UPDATE chat_invites SET person_addr = ? WHERE person_addr = ?`,
			`UPDATE chat_rooms SET created_by = ? WHERE created_by = ?`,
			`UPDATE chat_blobs SET uploader_addr = ? WHERE uploader_addr = ?`,
			`UPDATE chat_events SET sender_addr = ? WHERE sender_addr = ?`,
			`UPDATE chat_bot_messages SET sender_addr = ? WHERE sender_addr = ?`,
			`UPDATE chat_webhooks SET created_by = ? WHERE created_by = ?`,
		} {
			if _, err := tx.Exec(q, retired, addr); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE chat_participants SET person_addr = ? WHERE person_addr = ?`, addr, retired+"\x00tombstone"); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT id, dm_key FROM chat_rooms WHERE dm_key <> ''`)
		if err != nil {
			return err
		}
		keys := map[string]string{}
		for rows.Next() {
			var id, key string
			if err := rows.Scan(&id, &key); err != nil {
				rows.Close()
				return err
			}
			set := strings.Split(key, ",")
			hit := false
			for i, a := range set {
				if a == addr {
					set[i], hit = retired, true
				}
			}
			if hit {
				sort.Strings(set)
				keys[id] = strings.Join(set, ",")
			}
		}
		rows.Close()
		for id, key := range keys {
			if _, err := tx.Exec(`UPDATE chat_rooms SET dm_key = ? WHERE id = ?`, key, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteChatWebhooksBy(roomID, createdBy string) error {
	_, err := s.db.Exec(`DELETE FROM chat_webhooks WHERE room_id = ? AND created_by = ?`, roomID, createdBy)
	return err
}

func (s *Store) DeleteChatWebhook(id int64, roomID string) error {
	_, err := s.db.Exec(`DELETE FROM chat_webhooks WHERE id = ? AND room_id = ?`, id, roomID)
	return err
}

// ---- the bot's plaintext ----

type BotMessage struct {
	RoomID string `json:"roomId"`
	Seq    int64  `json:"seq"`
	ID     string `json:"id"`
	Sender string `json:"sender"`
	At     string `json:"at"`
	Text   string `json:"text"`
}

func (s *Store) PutBotMessage(m *BotMessage) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO chat_bot_messages (room_id, seq, id, sender_addr, at, text) VALUES (?, ?, ?, ?, ?, ?)`,
		m.RoomID, m.Seq, m.ID, m.Sender, m.At, m.Text)
	return err
}

// BotMessage returns one message the bot read, by room and message id.
func (s *Store) BotMessage(roomID, id string) (*BotMessage, error) {
	return ptr(queryOne(s.db, scanBotMessage, `SELECT room_id, seq, id, sender_addr, at, text FROM chat_bot_messages WHERE room_id = ? AND id = ?`, roomID, id))
}

// UpdateBotMessage replaces the text the bot holds for a message, after its
// author edited it.
func (s *Store) UpdateBotMessage(roomID, id, text string) error {
	_, err := s.db.Exec(`UPDATE chat_bot_messages SET text = ? WHERE room_id = ? AND id = ?`, text, roomID, id)
	return err
}

// DeleteBotMessage forgets one message the bot read, deleted in the room.
func (s *Store) DeleteBotMessage(roomID, id string) error {
	_, err := s.db.Exec(`DELETE FROM chat_bot_messages WHERE room_id = ? AND id = ?`, roomID, id)
	return err
}

// DeleteBotMessages forgets everything the bot read in a room it is no
// longer in.
func (s *Store) DeleteBotMessages(roomID string) error {
	_, err := s.db.Exec(`DELETE FROM chat_bot_messages WHERE room_id = ?`, roomID)
	return err
}

// BotMessages returns the newest messages in a room the bot read, oldest first.
func (s *Store) BotMessages(roomID string, limit int) ([]BotMessage, error) {
	return queryAll(s.db, scanBotMessage, `SELECT room_id, seq, id, sender_addr, at, text FROM (
		SELECT * FROM chat_bot_messages WHERE room_id = ? ORDER BY seq DESC LIMIT ?) ORDER BY seq`, roomID, limit)
}

func (s *Store) SearchBotMessages(q string, limit int) ([]BotMessage, error) {
	return queryAll(s.db, scanBotMessage, `SELECT room_id, seq, id, sender_addr, at, text FROM chat_bot_messages WHERE text LIKE ? ESCAPE '\' ORDER BY at DESC LIMIT ?`,
		"%"+likeEscape(q)+"%", limit)
}

func scanBotMessage(sc scanner) (BotMessage, error) {
	var m BotMessage
	err := sc.Scan(&m.RoomID, &m.Seq, &m.ID, &m.Sender, &m.At, &m.Text)
	return m, err
}
