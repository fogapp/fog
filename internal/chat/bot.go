package chat

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thomas-vilte/mls-go/ciphersuite"
	"github.com/thomas-vilte/mls-go/credentials"
	"github.com/thomas-vilte/mls-go/framing"
	"github.com/thomas-vilte/mls-go/group"
	"github.com/thomas-vilte/mls-go/keypackages"

	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// The fog bot (docs/chat-contract.md): fog as a member. It is a real MLS
// client — one device of the reserved person "fog" — whose keys live in
// fogd's vault, so fog can read and write exactly the rooms people put it in.
// It creates #fog, keeps up with commits in its rooms, mirrors notices, and
// is the only way anything server-side ever sees a message body.

const (
	BotHandle = "fog"
	// BotSecretKey is the vault secret holding the bot's ed25519 seed.
	BotSecretKey = "_chat_bot_key"
	// botKeyPackages is how many unused KeyPackages the bot keeps published.
	botKeyPackages = 8
	fogChannel     = "fog"
)

var botCS = ciphersuite.MLS128DHKEMX25519

// Bot is fog's own chat client.
type Bot struct {
	svc   *Service
	vault *vault.Vault
	dir   string

	person *store.Person
	device *store.PersonDevice
	actor  *Actor
	priv   ed25519.PrivateKey
	sigKey *ciphersuite.SignaturePrivateKey
	cred   *credentials.CredentialWithKey

	mu      sync.Mutex
	groups  map[string]*group.Group // by room id
	cursors map[string]int64        // last processed seq by room id
	// sentInEpoch and epochAt: the bot's messages in each room's current
	// epoch, and when that epoch began (as far as the bot knows): what
	// decides when it rolls the epoch (rollLocked).
	sentInEpoch map[string]int
	epochAt     map[string]time.Time
	pending     []pendingKP // published KeyPackages we still hold keys for
	// metaDirty: cursors moved past events that don't move the group's
	// epoch, and meta.json hasn't been written since (see flushMeta).
	metaDirty bool

	// OnMessage is called with each decrypted message in a room the bot is
	// in — the hook the MCP tools and webhooks use later.
	OnMessage func(room *store.ChatRoom, ev *store.ChatEvent, c Content)
}

type pendingKP struct {
	KP   []byte `json:"kp"`
	Init []byte `json:"init"`
	Enc  []byte `json:"enc"`
}

// NewBot loads or mints the bot's identity and registers it as a person and
// device. Safe to call on every start.
func NewBot(svc *Service, vlt *vault.Vault) (*Bot, error) {
	b := &Bot{svc: svc, vault: vlt, dir: filepath.Join(svc.DataDir, "chat", "bot"), groups: map[string]*group.Group{}, cursors: map[string]int64{}, sentInEpoch: map[string]int{}, epochAt: map[string]time.Time{}}
	if err := os.MkdirAll(filepath.Join(b.dir, "groups"), 0o700); err != nil {
		return nil, err
	}
	if err := b.loadKey(); err != nil {
		return nil, fmt.Errorf("bot key: %w", err)
	}
	pub := b.priv.Public().(ed25519.PublicKey)
	b.sigKey = ciphersuite.NewEd25519SignaturePrivateKey(b.priv)

	// The person row and its one device. The bot's identity key is its
	// device key, and it attests itself: fog vouches for its own device.
	p, err := svc.Store.PersonByHandle(BotHandle)
	if errors.Is(err, store.ErrNotFound) {
		if _, err := svc.Store.CreateBotPerson(BotHandle, "fog", []byte(pub)); err != nil {
			return nil, err
		}
		p, err = svc.Store.PersonByHandle(BotHandle)
	}
	if err != nil {
		return nil, fmt.Errorf("bot person: %w", err)
	}
	if len(p.IdentityKey) == 0 {
		svc.Store.SetPersonIdentityKey(p.ID, []byte(pub))
		p, _ = svc.Store.PersonByHandle(BotHandle)
	}
	b.person = p
	d, err := svc.Store.PersonDeviceByKey([]byte(pub))
	if errors.Is(err, store.ErrNotFound) {
		att := ed25519.Sign(b.priv, people.AttestationMessage([]byte(pub)))
		id, cerr := svc.Store.CreatePersonDevice(p.ID, "fog", "bot", []byte(pub), att)
		if cerr != nil {
			return nil, cerr
		}
		d, err = svc.Store.PersonDeviceByID(id)
	}
	if err != nil {
		return nil, fmt.Errorf("bot device: %w", err)
	}
	b.device = d
	addr, err := svc.Addr(BotHandle)
	if err != nil {
		return nil, fmt.Errorf("bot address: %w", err)
	}
	b.actor = &Actor{Person: p, Device: d, Addr: addr}
	b.cred = &credentials.CredentialWithKey{
		Credential:        credentials.NewBasicCredential([]byte(addr)),
		Ed25519PrivateKey: b.priv,
		SignatureKeyBytes: []byte(pub),
	}
	if err := b.loadState(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Bot) loadKey() error {
	secrets := vault.Secrets{Store: b.svc.Store, Vault: b.vault}
	seedB64, err := secrets.GetOrMint(0, BotSecretKey, func() (string, error) {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(priv.Seed()), nil
	})
	if err != nil {
		return err
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("bot key unusable")
	}
	b.priv = ed25519.NewKeyFromSeed(seed)
	return nil
}

// Addr is the bot's person address.
func (b *Bot) Addr() string { return b.actor.Addr }

// ---- persistence: every secret goes through the vault ----

func (b *Bot) writeSealed(name string, plain []byte) error {
	ct, err := b.vault.Encrypt(string(plain))
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(b.dir, name), []byte(ct), 0o600)
}

func (b *Bot) readSealed(name string) ([]byte, error) {
	ct, err := os.ReadFile(filepath.Join(b.dir, name))
	if err != nil {
		return nil, err
	}
	pt, err := b.vault.Decrypt(string(ct))
	if err != nil {
		return nil, err
	}
	return []byte(pt), nil
}

func (b *Bot) saveGroup(roomID string) error {
	g := b.groups[roomID]
	if g == nil {
		return nil
	}
	state, err := g.MarshalState()
	if err != nil {
		return err
	}
	return b.writeSealed(filepath.Join("groups", roomID+".state"), state)
}

func (b *Bot) saveMeta() error {
	meta, _ := json.Marshal(struct {
		Cursors map[string]int64 `json:"cursors"`
		Pending []pendingKP      `json:"pending"`
	}{b.cursors, b.pending})
	b.metaDirty = false
	return b.writeSealed("meta.json", meta)
}

// flushMeta writes meta.json if cursors moved since it was last written.
// Called once per catch-up page and on the run loop's tick, rather than
// per event: a lost cursor only replays messages, which the ratchet
// refuses to decrypt twice. Commits save at once (handleEvent), because
// replaying one the group already applied would drop the room.
func (b *Bot) flushMeta() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.metaDirty {
		if err := b.saveMeta(); err != nil {
			b.svc.Log.Warn("chat bot: save cursors", "err", err)
		}
	}
}

func (b *Bot) loadState() error {
	if meta, err := b.readSealed("meta.json"); err == nil {
		var m struct {
			Cursors map[string]int64 `json:"cursors"`
			Pending []pendingKP      `json:"pending"`
		}
		if json.Unmarshal(meta, &m) == nil {
			if m.Cursors != nil {
				b.cursors = m.Cursors
			}
			b.pending = m.Pending
		}
	}
	entries, _ := os.ReadDir(filepath.Join(b.dir, "groups"))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".state") {
			continue
		}
		roomID := strings.TrimSuffix(e.Name(), ".state")
		state, err := b.readSealed(filepath.Join("groups", e.Name()))
		if err != nil {
			b.svc.Log.Warn("chat bot: unreadable group state", "room", roomID, "err", err)
			continue
		}
		g, err := group.UnmarshalGroupState(state)
		if err != nil {
			b.svc.Log.Warn("chat bot: corrupt group state", "room", roomID, "err", err)
			continue
		}
		b.groups[roomID] = g
	}
	return nil
}

// ---- key packages ----

func (b *Bot) keyPackage() (*keypackages.KeyPackage, *keypackages.KeyPackagePrivateKeys, error) {
	return keypackages.Generate(b.cred, botCS)
}

// publishKeyPackages tops the bot's published KeyPackages up to the target.
func (b *Bot) publishKeyPackages() error {
	have, err := b.svc.Store.CountKeyPackages(b.device.ID)
	if err != nil {
		return err
	}
	var fresh [][]byte
	for have+len(fresh) < botKeyPackages {
		kp, priv, err := b.keyPackage()
		if err != nil {
			return err
		}
		raw := kp.Marshal()
		b.pending = append(b.pending, pendingKP{KP: raw, Init: priv.InitKey.Bytes(), Enc: priv.EncryptionKey.Bytes()})
		fresh = append(fresh, raw)
	}
	if len(fresh) == 0 {
		return nil
	}
	if err := b.svc.Store.AddKeyPackages(b.device.ID, int(botCS), fresh); err != nil {
		return err
	}
	return b.saveMeta()
}

// privateKeysFor rebuilds the private keys of a published KeyPackage.
func (b *Bot) privateKeysFor(p pendingKP) (*keypackages.KeyPackage, *keypackages.KeyPackagePrivateKeys, error) {
	kp, err := keypackages.UnmarshalKeyPackage(p.KP)
	if err != nil {
		return nil, nil, err
	}
	initKey, err := ecdh.X25519().NewPrivateKey(p.Init)
	if err != nil {
		return nil, nil, err
	}
	encKey, err := ecdh.X25519().NewPrivateKey(p.Enc)
	if err != nil {
		return nil, nil, err
	}
	return kp, &keypackages.KeyPackagePrivateKeys{InitKey: initKey, EncryptionKey: encKey, Ed25519SignatureKey: b.priv}, nil
}

// ---- rooms ----

// CreateRoom opens a room with the bot as its first member.
func (b *Bot) CreateRoom(in CreateRoomInput) (*store.ChatRoom, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	gidBytes := make([]byte, 16)
	id, _ := fed.NewULID()
	copy(gidBytes, id)
	kp, priv, err := b.keyPackage()
	if err != nil {
		return nil, err
	}
	g, err := group.NewGroup(group.NewGroupID(gidBytes), botCS, kp, priv)
	if err != nil {
		return nil, err
	}
	in.GroupID = people.EncodeKey(gidBytes)
	info, err := g.GetGroupInfoWithOptions(b.sigKey, group.WithRatchetTree(true), group.WithExternalPub(true))
	if err != nil {
		return nil, err
	}
	in.GroupInfo = people.EncodeKey(mlsview.WrapGroupInfo(info.Marshal()))
	room, err := b.svc.CreateRoom(b.actor, in)
	if err != nil {
		return nil, err
	}
	b.groups[room.ID] = g
	b.cursors[room.ID] = room.Seq
	if err := b.saveGroup(room.ID); err != nil {
		return nil, err
	}
	b.saveMeta()
	b.publishGroupInfo(room.ID)
	return room, nil
}

// publishGroupInfo uploads the current GroupInfo (with the tree and an
// external public key) so people can join the room externally.
func (b *Bot) publishGroupInfo(roomID string) {
	g := b.groups[roomID]
	if g == nil {
		return
	}
	info, err := g.GetGroupInfoWithOptions(b.sigKey, group.WithRatchetTree(true), group.WithExternalPub(true))
	if err != nil {
		b.svc.Log.Warn("chat bot: group info", "room", roomID, "err", err)
		return
	}
	wire := mlsview.WrapGroupInfo(info.Marshal())
	room, err := b.svc.Store.ChatRoom(roomID)
	if err != nil {
		return
	}
	if err := b.svc.SetGroupInfo(b.actor, room, int64(g.Epoch()), wire); err != nil {
		b.svc.Log.Warn("chat bot: publish group info", "room", roomID, "err", err)
	}
}

// EnsureFogChannel creates #fog if this fog has none yet.
func (b *Bot) EnsureFogChannel() (*store.ChatRoom, error) {
	rooms, err := b.svc.Store.ChatRoomsFor(b.actor.Addr)
	if err != nil {
		return nil, err
	}
	for i := range rooms {
		if rooms[i].Kind == KindChannel && rooms[i].Label == fogChannel && rooms[i].HubInstance == "" {
			if b.groups[rooms[i].ID] == nil {
				return nil, fmt.Errorf("chat bot: #fog exists but its group state is gone")
			}
			return &rooms[i], nil
		}
	}
	return b.CreateRoom(CreateRoomInput{Kind: KindChannel, Label: fogChannel, Topic: "What this fog is doing: deploys, updates, backups, peers."})
}

// Send posts content into a room the bot is in.
func (b *Bot) Send(roomID string, c Content) (*store.ChatEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groups[roomID]
	if g == nil {
		return nil, ErrNotParticipant
	}
	room, err := b.svc.Store.ChatRoom(roomID)
	if err != nil {
		return nil, err
	}
	// As members' clients do: nothing goes to the group while it still
	// holds leaves of someone the room has counted out, or of a revoked
	// device. A member's device removes them within moments; the bot waits.
	if b.svc.departedLeaves(room) {
		return nil, ErrAwaitingRemoval
	}
	if c.ID == "" {
		c.ID, _ = fed.NewULID()
	}
	if c.SentAt == "" {
		c.SentAt = time.Now().UTC().Format(time.RFC3339)
	}
	if c.System == "" {
		c.System = "fog"
	}
	plain, err := EncodeContent(c)
	if err != nil {
		return nil, err
	}
	pm, err := g.SendMessage(plain, b.sigKey)
	if err != nil {
		return nil, err
	}
	wire := framing.NewMLSMessagePrivate(pm).Marshal()
	hints := ""
	if len(c.Mentions) > 0 {
		h, _ := json.Marshal(map[string]any{HintMentions: c.Mentions})
		hints = string(h)
	}
	res, err := b.svc.Submit(b.actor, room, SubmitInput{ID: c.ID, Kind: EventPrivate, Payload: people.EncodeKey(wire), SentAt: c.SentAt, Hints: json.RawMessage(hints)})
	if err != nil {
		return nil, err
	}
	b.cursors[roomID] = res.Event.Seq
	b.saveGroup(roomID)
	b.saveMeta()
	// Well before a sender's chain gets too long for members' clients to
	// follow (100,000 in an epoch), or keys get old, the bot moves the room
	// to a fresh epoch.
	b.sentInEpoch[roomID]++
	if b.sentInEpoch[roomID] >= BotRollMessages || time.Since(b.epochSince(roomID)) >= BotRollAge {
		if err := b.rollLocked(room); err != nil {
			b.svc.Log.Warn("chat bot: rolling the epoch", "room", roomID, "err", err)
		}
	}
	return res.Event, nil
}

// When the bot moves a room it sends in to a fresh epoch: after this many
// of its messages in one epoch, or this long in one. Variables for tests.
var (
	BotRollMessages = 10_000
	BotRollAge      = 7 * 24 * time.Hour
)

func (b *Bot) epochSince(roomID string) time.Time {
	if t, ok := b.epochAt[roomID]; ok {
		return t
	}
	b.epochAt[roomID] = time.Now()
	return b.epochAt[roomID]
}

// rollLocked commits an empty commit with an update path — a new epoch,
// with fresh sending chains for everyone and fresh keys for the bot's leaf
// — and its GroupInfo, as the hub requires. Refused, the group goes back
// to what it was. The caller holds b.mu.
func (b *Bot) rollLocked(room *store.ChatRoom) error {
	g := b.groups[room.ID]
	if g == nil {
		return ErrNotParticipant
	}
	before, err := g.MarshalState()
	if err != nil {
		return err
	}
	undo := func() {
		if back, err := group.UnmarshalGroupState(before); err == nil {
			b.groups[room.ID] = back
		}
	}
	staged, err := g.Commit(b.sigKey, b.sigKey.PublicKey(), nil)
	if err != nil {
		return err
	}
	if err := g.MergeCommit(staged); err != nil {
		undo()
		return err
	}
	wire := framing.NewMLSMessagePublic(&framing.PublicMessage{
		Content:       staged.AuthenticatedContent().Content,
		Auth:          staged.AuthenticatedContent().Auth,
		MembershipTag: staged.MembershipTag(),
	}).Marshal()
	info, err := g.GetGroupInfoWithOptions(b.sigKey, group.WithRatchetTree(true), group.WithExternalPub(true))
	if err != nil {
		undo()
		return err
	}
	id, _ := fed.NewULID()
	res, err := b.svc.Submit(b.actor, room, SubmitInput{ID: id, Kind: EventCommit, Payload: people.EncodeKey(wire), GroupInfo: people.EncodeKey(mlsview.WrapGroupInfo(info.Marshal()))})
	if err != nil {
		undo()
		return err
	}
	b.cursors[room.ID] = res.Event.Seq
	b.sentInEpoch[room.ID] = 0
	b.epochAt[room.ID] = time.Now()
	b.saveGroup(room.ID)
	b.saveMeta()
	return nil
}

// Rooms lists the rooms the bot holds keys for.
func (b *Bot) Rooms() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.groups))
	for id := range b.groups {
		out = append(out, id)
	}
	return out
}

// ---- keeping up ----

// Run keeps the bot current: joins rooms it was invited to, processes
// commits, mirrors notices. Returns when ctx ends.
func (b *Bot) Run(ctx context.Context) {
	if err := b.publishKeyPackages(); err != nil {
		b.svc.Log.Warn("chat bot: key packages", "err", err)
	}
	if _, err := b.EnsureFogChannel(); err != nil {
		b.svc.Log.Warn("chat bot: #fog", "err", err)
	}
	b.CatchUp()
	sub := b.svc.Bus.Subscribe(b.actor.Addr, b.Rooms())
	defer b.svc.Bus.Unsubscribe(sub)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	defer b.flushMeta()
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-sub.C:
			switch n.Kind {
			case "event":
				if n.Event != nil {
					b.handleEvent(n.RoomID, n.Event)
				}
			case "rooms", "invite":
				b.CatchUp()
				sub.SetRooms(b.Rooms())
			}
		case <-tick.C:
			b.flushMeta()
			b.CatchUp()
			sub.SetRooms(b.Rooms())
			b.mirrorNotices()
			if err := b.publishKeyPackages(); err != nil {
				b.svc.Log.Warn("chat bot: key packages", "err", err)
			}
		}
	}
}

// CatchUp joins pending invites and processes every event the bot missed.
func (b *Bot) CatchUp() {
	b.acceptInvites()
	for _, roomID := range b.Rooms() {
		room, err := b.svc.Store.ChatRoom(roomID)
		if err != nil {
			continue
		}
		b.mu.Lock()
		after := b.cursors[roomID]
		b.mu.Unlock()
		for {
			evs, err := b.svc.Events(b.actor, room, after, 0, 200)
			if err != nil || len(evs) == 0 {
				break
			}
			for i := range evs {
				b.handleEvent(roomID, &evs[i])
				after = evs[i].Seq
			}
			b.flushMeta()
			if len(evs) < 200 {
				break
			}
		}
		b.mu.Lock()
		b.outOf(roomID)
		b.mu.Unlock()
	}
}

// acceptInvites joins rooms whose Welcome is addressed to the bot's device.
func (b *Bot) acceptInvites() {
	invs, err := b.svc.Store.ChatInvitesFor(b.actor.Addr)
	if err != nil {
		return
	}
	myKey := people.EncodeKey(b.device.DeviceKey)
	for _, inv := range invs {
		if inv.DeviceKey != "" && inv.DeviceKey != myKey {
			continue
		}
		b.mu.Lock()
		if b.groups[inv.RoomID] != nil {
			b.mu.Unlock()
			b.svc.Store.AcceptChatInvite(inv.ID, b.actor.Addr)
			continue
		}
		err := b.join(inv)
		b.mu.Unlock()
		if err != nil {
			b.svc.Log.Warn("chat bot: join", "room", inv.RoomID, "err", err)
			continue
		}
		b.svc.Store.AcceptChatInvite(inv.ID, b.actor.Addr)
	}
}

func (b *Bot) join(inv store.ChatInvite) error {
	msg, err := framing.UnmarshalMLSMessage(inv.Welcome)
	if err != nil || msg.Welcome == nil {
		return errors.New("welcome is not an MLSMessage")
	}
	welcome, err := group.UnmarshalWelcome(msg.Welcome)
	if err != nil {
		return err
	}
	var lastErr error = errors.New("no key package matched the welcome")
	for i, p := range b.pending {
		kp, priv, err := b.privateKeysFor(p)
		if err != nil {
			continue
		}
		g, err := group.JoinFromWelcome(welcome, kp, priv, nil)
		if err != nil {
			lastErr = err
			continue
		}
		b.groups[inv.RoomID] = g
		b.cursors[inv.RoomID] = inv.Seq
		b.pending = append(b.pending[:i], b.pending[i+1:]...)
		if err := b.saveGroup(inv.RoomID); err != nil {
			return err
		}
		b.saveMeta()
		b.svc.Log.Info("chat bot joined room", "room", inv.RoomID)
		return nil
	}
	return lastErr
}

// handleEvent applies one event from a room the bot is in.
func (b *Bot) handleEvent(roomID string, ev *store.ChatEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groups[roomID]
	if g == nil || ev.Seq <= b.cursors[roomID] {
		return
	}
	own := ev.SenderDevice == people.EncodeKey(b.device.DeviceKey)
	switch ev.Kind {
	case EventCommit:
		if own {
			break
		}
		msg, err := framing.UnmarshalMLSMessage(ev.Payload)
		if err != nil {
			break
		}
		pub, ok := msg.AsPublic()
		if !ok {
			break
		}
		if err := g.ProcessPublicMessage(pub); err != nil {
			if b.outOf(roomID) {
				return
			}
			b.svc.Log.Warn("chat bot: commit", "room", roomID, "seq", ev.Seq, "err", err)
			// The bot has fallen out of step with the group; it cannot
			// recover without a fresh Welcome. Forget the room so a member
			// can add it again.
			delete(b.groups, roomID)
			os.Remove(filepath.Join(b.dir, "groups", roomID+".state"))
			delete(b.cursors, roomID)
			b.saveMeta()
			return
		}
		b.saveGroup(roomID)
		b.sentInEpoch[roomID] = 0
		b.epochAt[roomID] = time.Now()
		// The epoch moved: the cursor must land with it (see flushMeta).
		b.cursors[roomID] = ev.Seq
		b.saveMeta()
		if b.outOf(roomID) {
			return
		}
		if room, err := b.svc.Store.ChatRoom(roomID); err == nil && room.Kind == KindChannel {
			b.mu.Unlock()
			b.publishGroupInfo(roomID)
			b.mu.Lock()
		}
		return
	case EventPrivate:
		if own {
			break
		}
		msg, err := framing.UnmarshalMLSMessage(ev.Payload)
		if err != nil {
			break
		}
		pm, ok := msg.AsPrivate()
		if !ok {
			break
		}
		plain, _, _, err := g.ReceiveApplicationMessage(pm)
		if err != nil {
			b.svc.Log.Debug("chat bot: cannot decrypt", "room", roomID, "seq", ev.Seq, "err", err)
			break
		}
		b.saveGroup(roomID)
		if c, err := DecodeContent(plain); err == nil {
			b.record(roomID, ev, c)
			b.applyContentRules(roomID, ev, c)
			if b.OnMessage != nil {
				if room, err := b.svc.Store.ChatRoom(roomID); err == nil {
					b.OnMessage(room, ev, c)
				}
			}
		}
	case EventSystem:
		// A departure may name the bot: banned, or its household cut.
		if b.outOf(roomID) {
			return
		}
	}
	b.cursors[roomID] = ev.Seq
	b.metaDirty = true
}

// outOf forgets a room the hub no longer counts the bot in — removed,
// banned, left — and everything the bot read there: its plaintext is fog's
// copy for webhooks, apps and MCP, and the room's members took fog's access
// away. Callers hold b.mu.
func (b *Bot) outOf(roomID string) bool {
	room, err := b.svc.Store.ChatRoom(roomID)
	if err != nil {
		return false
	}
	if _, err := b.svc.Participant(room, b.actor.Addr); err == nil {
		return false
	}
	delete(b.groups, roomID)
	delete(b.cursors, roomID)
	os.Remove(filepath.Join(b.dir, "groups", roomID+".state"))
	b.saveMeta()
	b.svc.Store.DeleteBotMessages(roomID)
	b.svc.Log.Info("chat bot left room", "room", roomID)
	return true
}

// applyContentRules keeps the bot's plaintext in line with the room's
// content rules: an author's edit replaces the text it holds; a delete by
// the author, or by someone whose role may delete others' messages, removes
// it. Callers hold b.mu.
func (b *Bot) applyContentRules(roomID string, ev *store.ChatEvent, c Content) {
	if c.Replaces == "" || (c.Type != "edit" && c.Type != "delete") {
		return
	}
	held, err := b.svc.Store.BotMessage(roomID, c.Replaces)
	if err != nil {
		return
	}
	author := held.Sender
	if i := strings.IndexByte(author, '#'); i >= 0 {
		author = author[:i] // the bot's own posts, noted with who spoke
	}
	switch c.Type {
	case "edit":
		if author == ev.SenderAddr && c.Body != nil {
			b.svc.Store.UpdateBotMessage(roomID, c.Replaces, c.Body.Text)
		}
	case "delete":
		if author == ev.SenderAddr || b.svc.Can(roomID, ev.SenderAddr, CapDeleteOtherMessage) {
			b.svc.Store.DeleteBotMessage(roomID, c.Replaces)
		}
	}
}

// mirrorNotices posts each new inbox notice into #fog once.
func (b *Bot) mirrorNotices() {
	room, err := b.fogRoom()
	if err != nil {
		return
	}
	notices, err := b.svc.Store.ActiveNotices()
	if err != nil {
		return
	}
	for _, n := range notices {
		key := fmt.Sprintf("%d", n.ID)
		if seen, _ := b.svc.Store.ChatBotSeen("notice", key); seen {
			continue
		}
		text := "**" + n.Title + "**"
		if n.Body != "" {
			text += "\n" + n.Body
		}
		if n.App != "" {
			text += "\n\n_" + n.App + "_"
		}
		if _, err := b.Send(room.ID, TextMessage("", text)); err != nil {
			b.svc.Log.Warn("chat bot: mirror notice", "err", err)
			return
		}
		b.svc.Store.MarkChatBotSeen("notice", key)
	}
}

// FogRoom returns #fog.
func (b *Bot) FogRoom() (*store.ChatRoom, error) { return b.fogRoom() }

func (b *Bot) fogRoom() (*store.ChatRoom, error) {
	rooms, err := b.svc.Store.ChatRoomsFor(b.actor.Addr)
	if err != nil {
		return nil, err
	}
	for i := range rooms {
		if rooms[i].Kind == KindChannel && rooms[i].Label == fogChannel && rooms[i].HubInstance == "" {
			return &rooms[i], nil
		}
	}
	return nil, store.ErrNotFound
}

// MirrorNoticesNow runs one notice-mirroring pass; the loop does this on
// its tick, tests and the inbox call it directly.
func (b *Bot) MirrorNoticesNow() { b.mirrorNotices() }

// PublishKeyPackagesNow tops up the bot's published KeyPackages.
func (b *Bot) PublishKeyPackagesNow() error { return b.publishKeyPackages() }

// record keeps the text of a message the bot read, for webhooks, apps and
// MCP tools. Only plain messages: reactions, edits and history bundles are
// folded away.
func (b *Bot) record(roomID string, ev *store.ChatEvent, c Content) {
	if c.Type != "message" || c.Body == nil {
		return
	}
	id := c.ID
	if id == "" {
		id = ev.ID
	}
	b.svc.Store.PutBotMessage(&store.BotMessage{RoomID: roomID, Seq: ev.Seq, ID: id, Sender: ev.SenderAddr, At: ev.HubAt, Text: c.Body.Text})
}

// SendAs posts text into a room the bot is in, on behalf of an integration
// (a webhook, an app, an assistant) that the content names.
func (b *Bot) SendAs(roomID, text, system string) (*store.ChatEvent, error) {
	// Send mints the content id on its own copy and submits the event under
	// it, so the record takes the id from the event, not from c.
	c := TextMessage("", text)
	// An integration's post is the integration's — an app's, a webhook's,
	// an assistant's — and members read it as such: it must never pass for
	// a notice from fog itself.
	if IntegrationStamp(system) {
		c.System = system
	}
	ev, err := b.Send(roomID, c)
	if err != nil {
		return nil, err
	}
	// Otherwise Send stamps System "fog"; the record notes who really spoke.
	sender := b.actor.Addr
	if system != "" {
		sender += "#" + system
	}
	b.svc.Store.PutBotMessage(&store.BotMessage{RoomID: roomID, Seq: ev.Seq, ID: ev.ID, Sender: sender, At: ev.HubAt, Text: text})
	return ev, nil
}

// InRoom says whether the bot holds keys for a room.
func (b *Bot) InRoom(roomID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.groups[roomID] != nil
}

// IntegrationStamp says a bot post's system stamp names an integration
// (app:, webhook:, mcp:) rather than fog itself.
func IntegrationStamp(system string) bool {
	for _, p := range []string{"app:", "webhook:", "mcp:"} {
		if strings.HasPrefix(system, p) && len(system) > len(p) {
			return true
		}
	}
	return false
}
