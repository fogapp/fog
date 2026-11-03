package mlsview

import (
	"context"
	"errors"
	"testing"

	mls "github.com/thomas-vilte/mls-go"
	"github.com/thomas-vilte/mls-go/ciphersuite"
	"github.com/thomas-vilte/mls-go/framing"
	"github.com/thomas-vilte/mls-go/group"
	"github.com/thomas-vilte/mls-go/keypackages"
)

// The tests drive real mls-go clients and check that the hub's keyless view
// lands every member on the same leaf the members themselves use.

type member struct {
	c  *mls.Client
	id string
}

func newMember(t *testing.T, id string) member {
	c, err := mls.NewClient([]byte(id), ciphersuite.MLS128DHKEMX25519)
	if err != nil {
		t.Fatal(err)
	}
	return member{c: c, id: id}
}

func kp(t *testing.T, m member) []byte {
	b, err := m.c.FreshKeyPackageBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parseKeyPackage is the leaf a raw KeyPackage would create.
func parseKeyPackage(b []byte) (*Leaf, error) {
	kp, err := keypackages.UnmarshalKeyPackage(b)
	if err != nil {
		return nil, err
	}
	return leafFromKeyPackage(kp)
}

// leavesOf reads the members' own view for comparison.
func leavesOf(t *testing.T, m member, gid []byte) map[uint32]string {
	list, err := m.c.ListMembers(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32]string{}
	for _, mi := range list {
		out[mi.LeafIndex] = string(mi.Identity)
	}
	return out
}

func viewOf(tr *Tree) map[uint32]string {
	out := map[uint32]string{}
	for _, l := range tr.Members() {
		out[l.Index] = l.Identity
	}
	return out
}

func same(a, b map[uint32]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestTreeFollowsRealCommits(t *testing.T) {
	ctx := context.Background()
	alice := newMember(t, "alice")
	gid, err := alice.c.CreateGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	creatorKP := kp(t, alice)
	creator, err := parseKeyPackage(creatorKP)
	if err != nil {
		t.Fatal(err)
	}
	// The creator's key package carries the same signature key as leaf 0.
	tr := NewTree(*creator)

	apply := func(commit []byte) []Change {
		m, err := Parse(commit)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !m.IsCommit() {
			t.Fatalf("not a commit")
		}
		ch, err := tr.Apply(m)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		return ch
	}

	// Add bob, carol, dave one at a time: leaves 1, 2, 3 (tree grows 1→2→4).
	others := []member{newMember(t, "bob"), newMember(t, "carol"), newMember(t, "dave")}
	var joined []member
	for _, o := range others {
		commit, welcome, err := alice.c.InviteMember(ctx, gid, kp(t, o))
		if err != nil {
			t.Fatal(err)
		}
		ch := apply(commit)
		if len(ch) != 1 || ch[0].Kind != "add" || ch[0].Leaf.Identity != o.id {
			t.Fatalf("changes for adding %s: %+v", o.id, ch)
		}
		// Earlier joiners process the commit before the newcomer joins.
		for _, p := range joined {
			if err := p.c.ProcessCommit(ctx, gid, commit); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := o.c.JoinGroup(ctx, welcome); err != nil {
			t.Fatal(err)
		}
		joined = append(joined, o)
	}
	if got, want := viewOf(tr), leavesOf(t, alice, gid); !same(got, want) {
		t.Fatalf("after adds: hub %v, member %v", got, want)
	}
	if len(tr.Leaves) != 4 {
		t.Errorf("tree size %d, want 4", len(tr.Leaves))
	}

	// Remove carol (leaf 2): blank, no truncation since dave holds leaf 3.
	commit, err := alice.c.RemoveMember(ctx, gid, []byte("carol"))
	if err != nil {
		t.Fatal(err)
	}
	ch := apply(commit)
	if len(ch) != 1 || ch[0].Kind != "remove" || ch[0].Leaf.Index != 2 {
		t.Fatalf("remove changes: %+v", ch)
	}
	for _, o := range others {
		if o.id != "carol" {
			if err := o.c.ProcessCommit(ctx, gid, commit); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, want := viewOf(tr), leavesOf(t, alice, gid); !same(got, want) {
		t.Fatalf("after remove: hub %v, member %v", got, want)
	}

	// Add eve: takes the blank leaf 2, not a new one.
	eve := newMember(t, "eve")
	commit, welcome, err := alice.c.InviteMember(ctx, gid, kp(t, eve))
	if err != nil {
		t.Fatal(err)
	}
	ch = apply(commit)
	if ch[0].Leaf.Index != 2 {
		t.Fatalf("eve should fill leaf 2, got %d", ch[0].Leaf.Index)
	}
	if _, err := eve.c.JoinGroup(ctx, welcome); err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		if o.id != "carol" {
			o.c.ProcessCommit(ctx, gid, commit)
		}
	}
	if got, want := viewOf(tr), leavesOf(t, alice, gid); !same(got, want) {
		t.Fatalf("after eve: hub %v, member %v", got, want)
	}

	// Remove dave (leaf 3) then eve (leaf 2): the tree truncates back to 2.
	for _, victim := range []string{"dave", "eve"} {
		commit, err := alice.c.RemoveMember(ctx, gid, []byte(victim))
		if err != nil {
			t.Fatal(err)
		}
		apply(commit)
		others[0].c.ProcessCommit(ctx, gid, commit)
		if victim == "dave" {
			eve.c.ProcessCommit(ctx, gid, commit)
		}
	}
	if len(tr.Leaves) != 2 {
		t.Errorf("tree size after truncation %d, want 2", len(tr.Leaves))
	}
	if got, want := viewOf(tr), leavesOf(t, alice, gid); !same(got, want) {
		t.Fatalf("after truncation: hub %v, member %v", got, want)
	}

	// A commit from bob (leaf 1) with nothing in it but his path keeps his
	// identity and, since only the encryption key rotates, changes no
	// membership.
	commit, _, err = others[0].c.CommitPendingProposals(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := Parse(commit)
	if m.SenderLeaf != 1 || m.Commit.PathLeaf == nil || m.Commit.PathLeaf.Identity != "bob" {
		t.Errorf("bob's commit: leaf %d path %+v", m.SenderLeaf, m.Commit.PathLeaf)
	}
	if ch = apply(commit); len(ch) != 0 {
		t.Fatalf("self-update changes: %+v", ch)
	}
	if got, want := viewOf(tr), leavesOf(t, others[0], gid); !same(got, want) {
		t.Fatalf("after self-update: hub %v, member %v", got, want)
	}
	if err := alice.c.ProcessCommit(ctx, gid, commit); err != nil {
		t.Fatal(err)
	}

	// External join lands at the leftmost free leaf (2) and is a
	// new_member_commit.
	frank := newMember(t, "frank")
	gi, err := alice.c.GroupInfo(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	_, commit, err = frank.c.ExternalJoin(ctx, gi)
	if err != nil {
		t.Fatal(err)
	}
	m, err = Parse(commit)
	if err != nil {
		t.Fatal(err)
	}
	if m.SenderType != framing.SenderTypeNewMemberCommit {
		t.Errorf("external commit sender type %d", m.SenderType)
	}
	ch, err = tr.Apply(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 1 || ch[0].Kind != "add" || ch[0].Leaf.Identity != "frank" || ch[0].Leaf.Index != 2 {
		t.Fatalf("external join changes: %+v", ch)
	}
	if err := alice.c.ProcessCommit(ctx, gid, commit); err != nil {
		t.Fatal(err)
	}
	if got, want := viewOf(tr), leavesOf(t, alice, gid); !same(got, want) {
		t.Fatalf("after external join: hub %v, member %v", got, want)
	}

	// Private messages parse to metadata only.
	ct, err := alice.c.SendMessage(ctx, gid, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	pm, err := Parse(ct)
	if err != nil {
		t.Fatal(err)
	}
	if !pm.IsPrivate() || pm.Commit != nil || string(pm.GroupID) != string(gid) {
		t.Errorf("private parse: %+v", pm)
	}
}

func TestRemoveBlankLeafRejected(t *testing.T) {
	tr := NewTree(Leaf{Identity: "a", SignatureKey: []byte("k")})
	if err := tr.remove(1); err != ErrBlankLeaf {
		t.Errorf("remove blank: %v", err)
	}
}

func TestTruncationSizes(t *testing.T) {
	tr := NewTree(Leaf{Identity: "a"})
	for _, id := range []string{"b", "c", "d", "e"} {
		tr.add(Leaf{Identity: id})
	}
	if len(tr.Leaves) != 8 {
		t.Fatalf("size %d want 8", len(tr.Leaves))
	}
	tr.remove(4) // e
	if len(tr.Leaves) != 4 {
		t.Errorf("size after removing e: %d want 4", len(tr.Leaves))
	}
	tr.remove(3)
	tr.remove(2)
	if len(tr.Leaves) != 2 {
		t.Errorf("size after removing d,c: %d want 2", len(tr.Leaves))
	}
	if idx := tr.add(Leaf{Identity: "f"}); idx != 2 || len(tr.Leaves) != 4 {
		t.Errorf("add after truncation: idx %d size %d", idx, len(tr.Leaves))
	}
}

// A commit that references a standalone proposal cannot be played — the hub
// keeps none to resolve it against — so it is refused, the tree untouched.
func TestApplyRefusesProposalRefs(t *testing.T) {
	tr := NewTree(Leaf{Identity: "a"})
	m := &Message{
		WireFormat: framing.WireFormatPublicMessage, ContentType: framing.ContentTypeCommit,
		SenderType: framing.SenderTypeMember, Commit: &Commit{Refs: [][]byte{{1, 2, 3}}},
	}
	if _, err := tr.Apply(m); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("apply with a ref: %v, want ErrUnsupported", err)
	}
	if len(tr.Members()) != 1 {
		t.Errorf("tree changed: %d members", len(tr.Members()))
	}
}

// An inline Update naming another member — the leaf takeover — is refused
// and moves nothing: inline, an Update is the committer's own, which RFC
// 9420 forbids. So is a commit removing its committer, and an Add that
// reuses a member's signature key.
func TestApplyRefusesWhatMembersRefuse(t *testing.T) {
	tr := NewTree(Leaf{Identity: "alice", SignatureKey: []byte("ka")})
	tr.add(Leaf{Identity: "bob", SignatureKey: []byte("kb")})
	member := func(sender uint32, props ...Proposal) *Message {
		return &Message{WireFormat: framing.WireFormatPublicMessage, ContentType: framing.ContentTypeCommit,
			SenderType: framing.SenderTypeMember, SenderLeaf: sender, Commit: &Commit{Proposals: props}}
	}
	for name, m := range map[string]*Message{
		"update of another's leaf":   member(1, Proposal{Type: group.ProposalTypeUpdate, Update: &Leaf{Identity: "alice", SignatureKey: []byte("kb2")}}),
		"remove of the committer":    member(1, Proposal{Type: group.ProposalTypeRemove, Remove: 1}),
		"add reusing a member's key": member(1, Proposal{Type: group.ProposalTypeAdd, Add: &Leaf{Identity: "carol", SignatureKey: []byte("ka")}}),
		"two removes of one leaf":    member(1, Proposal{Type: group.ProposalTypeRemove, Remove: 0}, Proposal{Type: group.ProposalTypeRemove, Remove: 0}),
		"a path renaming its leaf":   {WireFormat: framing.WireFormatPublicMessage, ContentType: framing.ContentTypeCommit, SenderType: framing.SenderTypeMember, SenderLeaf: 1, Commit: &Commit{PathLeaf: &Leaf{Identity: "alice", SignatureKey: []byte("kb")}}},
		"an external commit adding": {WireFormat: framing.WireFormatPublicMessage, ContentType: framing.ContentTypeCommit, SenderType: framing.SenderTypeNewMemberCommit,
			Commit: &Commit{PathLeaf: &Leaf{Identity: "eve", SignatureKey: []byte("ke")}, Proposals: []Proposal{{Type: group.ProposalTypeExternalInit}, {Type: group.ProposalTypeAdd, Add: &Leaf{Identity: "x", SignatureKey: []byte("kx")}}}}},
	} {
		if _, err := tr.Apply(m); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
	if got := viewOf(tr); got[0] != "alice" || got[1] != "bob" || string(tr.Leaf(0).SignatureKey) != "ka" || string(tr.Leaf(1).SignatureKey) != "kb" {
		t.Fatalf("tree moved: %+v", tr.Members())
	}
}
