package mlsview

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mls "github.com/thomas-vilte/mls-go"
	"github.com/thomas-vilte/mls-go/ciphersuite"
	"github.com/thomas-vilte/mls-go/framing"
	"github.com/thomas-vilte/mls-go/group"
	"github.com/thomas-vilte/mls-go/keypackages"
	"github.com/thomas-vilte/mls-go/storage/memory"
)

// The verified public state follows real commits exactly: after each, the
// GroupInfo a member publishes matches the hub's next GroupContext.
func TestPublicGroupFollowsRealCommits(t *testing.T) {
	ctx := context.Background()
	alice := newMember(t, "alice")
	gid, err := alice.c.CreateGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gi, _ := alice.c.GroupInfo(ctx, gid)
	pg, err := FromGroupInfo(WrapGroupInfo(gi))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	step := func(by member, commit []byte) {
		t.Helper()
		next, err := pg.Next(commit)
		if err != nil {
			t.Fatalf("an honest commit: %v", err)
		}
		// Stored and read back between commits, as the hub keeps it.
		js, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		if pg, err = UnmarshalPublicGroup(js); err != nil {
			t.Fatalf("read back: %v", err)
		}
		gi, err := by.c.GroupInfo(ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		if err := pg.CheckGroupInfo(WrapGroupInfo(gi)); err != nil {
			t.Fatalf("the committer's group info after: %v", err)
		}
	}
	bob, carol := newMember(t, "bob"), newMember(t, "carol")
	for _, o := range []member{bob, carol} {
		commit, welcome, err := alice.c.InviteMember(ctx, gid, kp(t, o))
		if err != nil {
			t.Fatal(err)
		}
		if o.id == "carol" {
			bob.c.ProcessCommit(ctx, gid, commit)
		}
		step(alice, commit)
		o.c.JoinGroup(ctx, welcome)
	}
	upd, _, err := bob.c.CommitPendingProposals(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	step(bob, upd)
	alice.c.ProcessCommit(ctx, gid, upd)
	carol.c.ProcessCommit(ctx, gid, upd)
	rm, err := alice.c.RemoveMember(ctx, gid, []byte("carol"))
	if err != nil {
		t.Fatal(err)
	}
	step(alice, rm)
	bob.c.ProcessCommit(ctx, gid, rm)
	dave := newMember(t, "dave")
	gi, _ = alice.c.GroupInfo(ctx, gid)
	_, ext, err := dave.c.ExternalJoin(ctx, gi)
	if err != nil {
		t.Fatal(err)
	}
	step(dave, ext)
	if got := len(pg.Members()); got != 3 {
		t.Fatalf("members after the run: %d", got)
	}
}

// A commit with a broken signature, or whose path breaks the tree's parent
// hashes, does not verify; nor does a GroupInfo for another epoch or state.
func TestPublicGroupRefusesForgeries(t *testing.T) {
	ctx := context.Background()
	alice, bob := newMember(t, "alice"), newMember(t, "bob")
	gid, _ := alice.c.CreateGroup(ctx)
	gi0, _ := alice.c.GroupInfo(ctx, gid)
	pg, err := FromGroupInfo(WrapGroupInfo(gi0))
	if err != nil {
		t.Fatal(err)
	}
	commit, _, _ := alice.c.InviteMember(ctx, gid, kp(t, bob))

	mm, _ := framing.UnmarshalMLSMessage(commit)
	pm, _ := mm.AsPublic()
	sig := append([]byte(nil), pm.Auth.Signature.AsSlice()...)
	sig[3] ^= 0xff
	pm.Auth.Signature = ciphersuite.NewSignature(sig)
	badSig := framing.NewMLSMessagePublic(pm).Marshal()
	pm.Auth.Signature = nil
	for name, c := range map[string][]byte{"flipped byte": badSig, "unsigned": framing.NewMLSMessagePublic(pm).Marshal()} {
		if _, err := pg.Next(c); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	// A GroupInfo for epoch 0 against the state after the add.
	next, err := pg.Next(commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.CheckGroupInfo(WrapGroupInfo(gi0)); !errors.Is(err, ErrUnverified) {
		t.Errorf("a stale group info: %v", err)
	}
	// A GroupInfo signed for the right epoch but by a non-member.
	mallory := newMember(t, "mallory")
	mgid, _ := mallory.c.CreateGroup(ctx)
	mgi, _ := mallory.c.GroupInfo(ctx, mgid)
	if err := pg.CheckGroupInfo(WrapGroupInfo(mgi)); err == nil {
		t.Error("another group's group info verified")
	}
}

// keyCatcher keeps the signature key a client stores, so a test can sign
// what that member crafts by hand — the hub's hard case: a member's own
// valid signature over a commit that is wrong inside.
type keyCatcher struct {
	group.KeyStore
	key *ciphersuite.SignaturePrivateKey
}

func (k *keyCatcher) StoreSignatureKey(ctx context.Context, id *group.ID, key *ciphersuite.SignaturePrivateKey) error {
	k.key = key
	return k.KeyStore.StoreSignatureKey(ctx, id, key)
}

// A member signs, with their own key, a commit whose path breaks the tree's
// parent hashes, and one whose added key package isn't signed by its leaf.
// The signature is good; the commit is still refused.
func TestPublicGroupRefusesSignedForgeries(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewStore()
	kc := &keyCatcher{KeyStore: mem}
	ac, err := mls.NewClient([]byte("alice"), ciphersuite.MLS128DHKEMX25519, mls.WithStorage(mem, kc))
	if err != nil {
		t.Fatal(err)
	}
	alice := member{c: ac, id: "alice"}
	gid, _ := alice.c.CreateGroup(ctx)
	gi, _ := alice.c.GroupInfo(ctx, gid)
	pg, err := FromGroupInfo(WrapGroupInfo(gi))
	if err != nil {
		t.Fatal(err)
	}
	if kc.key == nil {
		t.Fatal("no signature key caught")
	}
	bob := newMember(t, "bob")
	add, welcome, _ := alice.c.InviteMember(ctx, gid, kp(t, bob))
	bob.c.JoinGroup(ctx, welcome)
	// resign has alice re-sign commit with change made: it verifies as hers.
	resign := func(at *PublicGroup, commit []byte, change func(c *group.Commit)) []byte {
		mm, _ := framing.UnmarshalMLSMessage(commit)
		pm, _ := mm.AsPublic()
		data, _ := pm.Content.CommitData()
		c, err := group.UnmarshalCommit(data)
		if err != nil {
			t.Fatal(err)
		}
		change(c)
		pm.Content.Body = framing.CommitBody{Data: c.Marshal()}
		auth := &framing.AuthenticatedContent{WireFormat: framing.WireFormatPublicMessage, Content: pm.Content, GroupContext: at.context.Marshal()}
		sig, err := ciphersuite.SignWithLabel(kc.key, "FramedContentTBS", auth.MarshalTBS())
		if err != nil {
			t.Fatal(err)
		}
		pm.Auth.Signature = sig
		return framing.NewMLSMessagePublic(pm).Marshal()
	}
	other := newMember(t, "other")
	okp, _ := keypackages.UnmarshalKeyPackage(kp(t, other))
	// Re-signing an honest commit unchanged still verifies: the forgeries
	// fail on what they change, not on the signing.
	if _, err := pg.Next(resign(pg, add, func(*group.Commit) {})); err != nil {
		t.Fatalf("an honest commit, re-signed: %v", err)
	}
	forged := resign(pg, add, func(c *group.Commit) {
		for _, p := range c.Proposals {
			if p.Proposal != nil && p.Proposal.Add != nil {
				p.Proposal.Add.KeyPackage.Signature = append([]byte(nil), okp.Signature...)
			}
		}
	})
	if _, err := pg.Next(forged); !errors.Is(err, ErrUnverified) {
		t.Errorf("an add whose key package isn't signed by its leaf: %v", err)
	}
	pg, err = pg.Next(add)
	if err != nil {
		t.Fatal(err)
	}
	// With two members, a commit's path carries a node: break it.
	refresh, _, err := alice.c.CommitPendingProposals(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Next(resign(pg, refresh, func(*group.Commit) {})); err != nil {
		t.Fatalf("an honest key refresh, re-signed: %v", err)
	}
	for name, change := range map[string]func(c *group.Commit){
		"a path key that breaks the parent hashes": func(c *group.Commit) {
			c.Path.Nodes[0].EncryptionKey = okp.InitKey
		},
		"a path with a node too few": func(c *group.Commit) {
			c.Path.Nodes = nil
		},
		"a path leaf that keeps its old key": func(c *group.Commit) {
			c.Path.LeafNode.EncryptionKey = pg.tree.GetLeaf(0).LeafData.EncryptionKey
		},
	} {
		if _, err := pg.Next(resign(pg, refresh, change)); !errors.Is(err, ErrUnverified) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
