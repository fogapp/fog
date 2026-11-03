// Package mlsview is the hub's view of an MLS group without a key to it.
//
// A fog that hosts a room must know who is in it — to enforce policy, to fan
// out to the right households, to show the participant list — but must never
// hold the group's secrets. MLS makes that possible: membership changes are
// commits carried as PublicMessages, and the ratchet tree's leaves are public.
// This package parses those messages (with mls-go's wire codecs) and keeps a
// leaf-indexed list of members that follows RFC 9420's placement rules, so the
// hub always knows which leaf is which device.
//
// It deliberately does not verify commit signatures or confirmation tags —
// that needs the GroupContext, which needs the tree hash, which is the members'
// job and one they cannot skip. The hub's authorization comes from the
// authenticated transport: a device says "leaf 3 is me", and the hub checks
// that leaf 3's signature key is that device's registered key.
package mlsview

import (
	"errors"
	"fmt"

	"github.com/thomas-vilte/mls-go/framing"
	"github.com/thomas-vilte/mls-go/group"
	"github.com/thomas-vilte/mls-go/keypackages"
	"github.com/thomas-vilte/mls-go/treesync"
)

var (
	ErrNotPublic     = errors.New("mls: not a public message")
	ErrBlankLeaf     = errors.New("mls: proposal names a blank leaf")
	ErrBadSender     = errors.New("mls: sender leaf is blank")
	ErrNoPathLeaf    = errors.New("mls: external commit carries no leaf node")
	ErrUnsupported   = errors.New("mls: unsupported proposal in commit")
	ErrIdentityMoved = errors.New("mls: committer's path changes its identity")
	// ErrInvalidCommit is a commit RFC 9420 §12.2 forbids: every member
	// would refuse it, so a hub that played it would part from the group.
	ErrInvalidCommit = errors.New("mls: invalid commit")
)

// Leaf is one member of the group: the credential identity and the leaf's
// signature key, which for fog is the device key registered under a person.
type Leaf struct {
	Index        uint32 `json:"index"`
	Identity     string `json:"identity"`
	SignatureKey []byte `json:"signatureKey"`
	// Departed: the device this leaf belongs to left the room by itself
	// (its person stays): any member may remove the leaf.
	Departed bool `json:"departed,omitempty"`
}

// Tree is the public leaf list. len(Leaves) is 0 or a power of two, as the
// real tree's leaf count is; nil entries are blank leaves.
type Tree struct {
	Leaves []*Leaf
}

// NewTree seeds a group whose creator sits at leaf 0.
func NewTree(creator Leaf) *Tree {
	creator.Index = 0
	return &Tree{Leaves: []*Leaf{&creator}}
}

// Members lists the non-blank leaves in index order.
func (t *Tree) Members() []Leaf {
	var out []Leaf
	for _, l := range t.Leaves {
		if l != nil {
			out = append(out, *l)
		}
	}
	return out
}

func (t *Tree) Leaf(i uint32) *Leaf {
	if int(i) >= len(t.Leaves) {
		return nil
	}
	return t.Leaves[i]
}

// FindKey returns the leaf holding a signature key, or nil.
func (t *Tree) FindKey(key []byte) *Leaf {
	for _, l := range t.Leaves {
		if l != nil && string(l.SignatureKey) == string(key) {
			return l
		}
	}
	return nil
}

// add places a member at the leftmost blank leaf, extending (doubling) the
// tree when there is none — RFC 9420 §12.1.1.
func (t *Tree) add(l Leaf) uint32 {
	for i, cur := range t.Leaves {
		if cur == nil {
			l.Index = uint32(i)
			t.Leaves[i] = &l
			return l.Index
		}
	}
	n := len(t.Leaves)
	if n == 0 {
		l.Index = 0
		t.Leaves = []*Leaf{&l}
		return 0
	}
	grown := make([]*Leaf, n*2)
	copy(grown, t.Leaves)
	l.Index = uint32(n)
	grown[n] = &l
	t.Leaves = grown
	return l.Index
}

// remove blanks a leaf and truncates the right subtree while it is all blank
// — RFC 9420 §12.1.3.
func (t *Tree) remove(i uint32) error {
	if int(i) >= len(t.Leaves) || t.Leaves[i] == nil {
		return ErrBlankLeaf
	}
	t.Leaves[i] = nil
	t.truncate()
	return nil
}

func (t *Tree) truncate() {
	for len(t.Leaves) > 1 {
		half := len(t.Leaves) / 2
		empty := true
		for _, l := range t.Leaves[half:] {
			if l != nil {
				empty = false
				break
			}
		}
		if !empty {
			return
		}
		t.Leaves = t.Leaves[:half]
	}
}

// Proposal is the part of an MLS proposal the hub acts on.
type Proposal struct {
	Type group.ProposalType
	// Add carries the new member's leaf (index unset until applied).
	Add *Leaf
	// Remove is the leaf index to blank.
	Remove uint32
	// Update carries the sender's replacement leaf.
	Update *Leaf
	// Extensions is the new GroupContext extension list (GroupContextExtensions).
	Extensions []group.Extension
	// Raw is the proposal's wire encoding, for hashing into a ProposalRef.
	Raw []byte
}

// Commit is the part of an MLS Commit the hub acts on.
type Commit struct {
	Proposals []Proposal
	Refs      [][]byte
	// PathLeaf is the committer's new leaf node when the commit has a path.
	PathLeaf *Leaf
}

// Message is a parsed MLSMessage. Only the fields a courier is allowed to
// see are populated.
type Message struct {
	WireFormat  framing.WireFormat
	GroupID     []byte
	Epoch       uint64
	ContentType framing.ContentType
	// Public-message fields.
	SenderType framing.SenderType
	SenderLeaf uint32
	Commit     *Commit
	Proposal   *Proposal
	// AuthenticatedData is the unencrypted AAD, on both public and private
	// messages.
	AuthenticatedData []byte
	// Raw is the exact bytes parsed.
	Raw []byte
}

func (m *Message) IsPrivate() bool { return m.WireFormat == framing.WireFormatPrivateMessage }

// IsApplication says a private message carries application data. Its
// content type travels in the clear (RFC 9420 §6.3), so the hub can tell an
// encrypted commit or proposal — a membership change it could not check —
// from a message.
func (m *Message) IsApplication() bool {
	return m.IsPrivate() && m.ContentType == framing.ContentTypeApplication
}
func (m *Message) IsCommit() bool {
	return m.WireFormat == framing.WireFormatPublicMessage && m.ContentType == framing.ContentTypeCommit
}
func (m *Message) IsProposal() bool {
	return m.WireFormat == framing.WireFormatPublicMessage && m.ContentType == framing.ContentTypeProposal
}

// Parse decodes an MLSMessage. Private messages yield group id, epoch, and
// AAD only; public ones are fully parsed.
func Parse(b []byte) (*Message, error) {
	mm, err := framing.UnmarshalMLSMessage(b)
	if err != nil {
		return nil, fmt.Errorf("mls: %w", err)
	}
	m := &Message{WireFormat: mm.WireFormat(), Raw: b}
	if pm, ok := mm.AsPrivate(); ok {
		m.GroupID = pm.GroupID
		m.Epoch = pm.Epoch
		m.ContentType = pm.ContentType
		m.AuthenticatedData = pm.AuthenticatedData
		return m, nil
	}
	pub, ok := mm.AsPublic()
	if !ok {
		return m, nil
	}
	fc := pub.Content
	m.GroupID = fc.GroupID
	m.Epoch = fc.Epoch
	m.ContentType = fc.ContentType()
	m.SenderType = fc.Sender.Type
	m.SenderLeaf = fc.Sender.LeafIndex
	m.AuthenticatedData = fc.AuthenticatedData
	switch m.ContentType {
	case framing.ContentTypeCommit:
		data, _ := fc.CommitData()
		c, err := group.UnmarshalCommit(data)
		if err != nil {
			return nil, fmt.Errorf("mls: commit: %w", err)
		}
		m.Commit = &Commit{}
		for _, por := range c.Proposals {
			if por.Proposal != nil {
				p, err := convertProposal(por.Proposal)
				if err != nil {
					return nil, err
				}
				m.Commit.Proposals = append(m.Commit.Proposals, p)
			} else {
				m.Commit.Refs = append(m.Commit.Refs, por.ProposalRef)
			}
		}
		if c.Path != nil && c.Path.LeafNode != nil {
			l, err := leafFromData(c.Path.LeafNode)
			if err != nil {
				return nil, err
			}
			m.Commit.PathLeaf = l
		}
	case framing.ContentTypeProposal:
		data, _ := fc.ProposalData()
		p, err := group.UnmarshalProposal(data)
		if err != nil {
			return nil, fmt.Errorf("mls: proposal: %w", err)
		}
		cp, err := convertProposal(p)
		if err != nil {
			return nil, err
		}
		m.Proposal = &cp
	}
	return m, nil
}

func convertProposal(p *group.Proposal) (Proposal, error) {
	out := Proposal{Type: p.Type, Raw: group.ProposalMarshal(p)}
	switch p.Type {
	case group.ProposalTypeAdd:
		if p.Add == nil || p.Add.KeyPackage == nil {
			return out, errors.New("mls: add without key package")
		}
		l, err := leafFromKeyPackage(p.Add.KeyPackage)
		if err != nil {
			return out, err
		}
		out.Add = l
	case group.ProposalTypeRemove:
		if p.Remove == nil {
			return out, errors.New("mls: remove without index")
		}
		out.Remove = uint32(p.Remove.Removed)
	case group.ProposalTypeUpdate:
		if p.Update != nil && p.Update.LeafNode != nil && p.Update.LeafNode.Credential != nil {
			out.Update = &Leaf{
				Identity:     string(p.Update.LeafNode.Credential.Identity),
				SignatureKey: append([]byte(nil), p.Update.LeafNode.SignatureKeyBytes...),
			}
		}
	case group.ProposalTypeGroupContextExtensions:
		if p.GroupContextExtensions != nil {
			out.Extensions = p.GroupContextExtensions.Extensions
		}
	case group.ProposalTypePreSharedKey, group.ProposalTypeExternalInit, group.ProposalTypeReInit:
		// Nothing for the hub to track; the members handle the key schedule.
	}
	return out, nil
}

func leafFromKeyPackage(kp *keypackages.KeyPackage) (*Leaf, error) {
	if kp.LeafNode == nil || kp.LeafNode.Credential == nil {
		return nil, errors.New("mls: key package without credential")
	}
	return &Leaf{
		Identity:     string(kp.LeafNode.Credential.Identity),
		SignatureKey: append([]byte(nil), kp.LeafNode.SignatureKeyBytes...),
	}, nil
}

func leafFromData(ln *treesync.LeafNodeData) (*Leaf, error) {
	if ln.Credential == nil {
		return nil, errors.New("mls: leaf node without credential")
	}
	return &Leaf{
		Identity:     string(ln.Credential.Identity),
		SignatureKey: append([]byte(nil), ln.SignatureKeyRaw...),
	}, nil
}

// Change is one membership effect of a commit.
type Change struct {
	Kind string // add | remove | update
	Leaf Leaf
}

// Check holds a commit to the proposal-list rules of RFC 9420 §12.2 that
// decide which leaves it touches, against the tree as it stands — the rules
// whose breach would have the hub play a commit every member refuses. It
// changes nothing; Apply calls it first.
//
// Only inline proposals exist here: the hub keeps no standalone ones, so a
// commit that references one is refused. An inline Update is always the
// committer's own (§12.4: a proposal by value is the committer's), which
// §12.2 forbids — a committer replaces its leaf through its path — so no
// commit the hub plays ever replaces a leaf other than the committer's.
func (t *Tree) Check(m *Message) error {
	if !m.IsCommit() {
		return ErrNotPublic
	}
	c := m.Commit
	if len(c.Refs) > 0 {
		return ErrUnsupported
	}
	count := map[group.ProposalType]int{}
	removed := map[uint32]bool{}
	for _, p := range c.Proposals {
		count[p.Type]++
		switch p.Type {
		case group.ProposalTypeAdd, group.ProposalTypeRemove, group.ProposalTypeGroupContextExtensions, group.ProposalTypeExternalInit:
		case group.ProposalTypeUpdate:
			return fmt.Errorf("%w: an Update inside a commit is the committer's own, which RFC 9420 forbids; a committer updates its leaf through its path", ErrInvalidCommit)
		default:
			// PreSharedKey and ReInit: fog uses neither, so no member's
			// client would make one.
			return fmt.Errorf("%w: proposal type %d", ErrUnsupported, p.Type)
		}
		if p.Type == group.ProposalTypeRemove {
			if t.Leaf(p.Remove) == nil {
				return ErrBlankLeaf
			}
			if removed[p.Remove] {
				return fmt.Errorf("%w: two removes of leaf %d", ErrInvalidCommit, p.Remove)
			}
			removed[p.Remove] = true
		}
	}
	switch m.SenderType {
	case framing.SenderTypeMember:
		if t.Leaf(m.SenderLeaf) == nil {
			return ErrBadSender
		}
		if removed[m.SenderLeaf] {
			return fmt.Errorf("%w: a commit cannot remove its committer", ErrInvalidCommit)
		}
		if count[group.ProposalTypeExternalInit] > 0 {
			return fmt.Errorf("%w: external init in a member's commit", ErrInvalidCommit)
		}
		if count[group.ProposalTypeGroupContextExtensions] > 1 {
			return fmt.Errorf("%w: more than one group context extensions proposal", ErrInvalidCommit)
		}
		if c.PathLeaf != nil && c.PathLeaf.Identity != t.Leaf(m.SenderLeaf).Identity {
			return ErrIdentityMoved
		}
	case framing.SenderTypeNewMemberCommit:
		if c.PathLeaf == nil {
			return ErrNoPathLeaf
		}
		if count[group.ProposalTypeExternalInit] != 1 || count[group.ProposalTypeRemove] > 1 ||
			count[group.ProposalTypeAdd] > 0 || count[group.ProposalTypeGroupContextExtensions] > 0 {
			return fmt.Errorf("%w: an external commit carries one external init and at most one remove", ErrInvalidCommit)
		}
	default:
		return fmt.Errorf("%w: sender type %d", ErrInvalidCommit, m.SenderType)
	}
	// Every leaf's signature key is unique (§7.3): an Add or a path leaf
	// may not reuse the key of a leaf that stays, or of another Add.
	keys := map[string]bool{}
	for _, l := range t.Leaves {
		if l != nil && !removed[l.Index] && !(m.SenderType == framing.SenderTypeMember && l.Index == m.SenderLeaf) {
			keys[string(l.SignatureKey)] = true
		}
	}
	claim := func(l *Leaf) error {
		if keys[string(l.SignatureKey)] {
			return fmt.Errorf("%w: a leaf with that signature key is already in the group", ErrInvalidCommit)
		}
		keys[string(l.SignatureKey)] = true
		return nil
	}
	if m.SenderType == framing.SenderTypeMember {
		cur := t.Leaf(m.SenderLeaf).SignatureKey
		if c.PathLeaf != nil {
			cur = c.PathLeaf.SignatureKey
		}
		if err := claim(&Leaf{SignatureKey: cur}); err != nil {
			return err
		}
	} else if err := claim(c.PathLeaf); err != nil {
		return err
	}
	for _, p := range c.Proposals {
		if p.Type == group.ProposalTypeAdd {
			if err := claim(p.Add); err != nil {
				return err
			}
		}
	}
	return nil
}

// Apply plays a commit against the tree in RFC 9420 §12.3 order —
// Removes, then Adds — then places an external joiner or replaces the
// committer's leaf from the path. A commit Check refuses changes nothing.
func (t *Tree) Apply(m *Message) ([]Change, error) {
	if err := t.Check(m); err != nil {
		return nil, err
	}
	c := m.Commit
	var changes []Change
	// Removes.
	for _, p := range c.Proposals {
		if p.Type != group.ProposalTypeRemove {
			continue
		}
		removed := *t.Leaf(p.Remove)
		if err := t.remove(p.Remove); err != nil {
			return nil, err
		}
		changes = append(changes, Change{Kind: "remove", Leaf: removed})
	}
	// Adds.
	for _, p := range c.Proposals {
		if p.Type != group.ProposalTypeAdd {
			continue
		}
		idx := t.add(*p.Add)
		changes = append(changes, Change{Kind: "add", Leaf: *t.Leaves[idx]})
	}
	// The committer: its own leaf, and only its own, from the path.
	switch m.SenderType {
	case framing.SenderTypeMember:
		cur := t.Leaf(m.SenderLeaf)
		if c.PathLeaf != nil {
			nl := *c.PathLeaf
			nl.Index = cur.Index
			if string(nl.SignatureKey) != string(cur.SignatureKey) {
				changes = append(changes, Change{Kind: "update", Leaf: nl})
			}
			*cur = nl
		}
	case framing.SenderTypeNewMemberCommit:
		idx := t.add(*c.PathLeaf)
		changes = append(changes, Change{Kind: "add", Leaf: *t.Leaves[idx]})
	}
	return changes, nil
}

// WrapGroupInfo frames a raw GroupInfo as an MLSMessage — the only shape
// that crosses fog's wire, and the one every RFC 9420 client decodes.
func WrapGroupInfo(raw []byte) []byte {
	return (&framing.MLSMessage{GroupInfo: raw}).Marshal()
}

// UnwrapGroupInfo returns the GroupInfo inside an MLSMessage, refusing
// anything that is not one.
func UnwrapGroupInfo(wire []byte) ([]byte, error) {
	mm, err := framing.UnmarshalMLSMessage(wire)
	if err != nil {
		return nil, fmt.Errorf("mls: group info: %w", err)
	}
	if mm.WireFormat() != framing.WireFormatGroupInfo || len(mm.GroupInfo) == 0 {
		return nil, errors.New("mls: not a group info message")
	}
	if _, err := group.UnmarshalGroupInfo(mm.GroupInfo); err != nil {
		return nil, fmt.Errorf("mls: group info: %w", err)
	}
	return mm.GroupInfo, nil
}
