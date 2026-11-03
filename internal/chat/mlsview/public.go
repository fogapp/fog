package mlsview

// The hub's public half of the group, verified.
//
// Everything in an MLS commit that its signer can be held to without the
// group's secrets, the hub checks before it orders the commit: the
// signature over the content and the current GroupContext; every new leaf's
// signature; the new tree's parent-hash chains; and from the new tree hash
// and the confirmed transcript hash, the next GroupContext, which every
// GroupInfo published for that epoch must match and carry the signature of
// one of its leaves. To do that the hub keeps what an external joiner would
// hold — the full public ratchet tree, the GroupContext and the interim
// transcript hash — seeded from the creator's GroupInfo at epoch 0 and
// advanced only by commits it verified, never by a member's say-so.
//
// What needs the group's secrets stays the members' to check: the
// confirmation tag and membership tag (keyed by epoch secrets), and that the
// encrypted path secrets, an external join's KEM output or a GroupInfo's
// external_pub decrypt to anything.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thomas-vilte/mls-go/ciphersuite"
	"github.com/thomas-vilte/mls-go/extensions"
	"github.com/thomas-vilte/mls-go/framing"
	"github.com/thomas-vilte/mls-go/group"
	"github.com/thomas-vilte/mls-go/treesync"
)

// ErrUnverified is a commit or GroupInfo that doesn't verify against the
// group's public state.
var ErrUnverified = errors.New("mls: does not verify against the group")

// PublicGroup is a group's verifiable public state at one epoch.
type PublicGroup struct {
	cs      ciphersuite.CipherSuite
	context *group.GroupContext
	interim []byte
	tree    *treesync.RatchetTree
}

type publicJSON struct {
	Context []byte `json:"context"`
	Interim []byte `json:"interim"`
	Tree    []byte `json:"tree"`
}

// MarshalJSON stores the state.
func (pg *PublicGroup) MarshalJSON() ([]byte, error) {
	return json.Marshal(publicJSON{Context: pg.context.Marshal(), Interim: pg.interim, Tree: pg.tree.MarshalTreeRFC()})
}

// UnmarshalPublicGroup reads stored state back.
func UnmarshalPublicGroup(b []byte) (*PublicGroup, error) {
	var j publicJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	gc, err := group.UnmarshalGroupContext(j.Context)
	if err != nil {
		return nil, err
	}
	tree, err := treesync.UnmarshalTreeFromExtension(j.Tree, gc.CipherSuite)
	if err != nil {
		return nil, err
	}
	tree.SetCipherSuite(gc.CipherSuite)
	tree = tree.ExpandToPowerOf2()
	return &PublicGroup{cs: gc.CipherSuite, context: gc, interim: j.Interim, tree: tree}, nil
}

// Epoch is the epoch the state is at.
func (pg *PublicGroup) Epoch() uint64 { return pg.context.Epoch.AsUint64() }

// GroupID is the group's id.
func (pg *PublicGroup) GroupID() []byte { return pg.context.GroupID.AsSlice() }

// Members lists the leaves, as the keyless view keeps them.
func (pg *PublicGroup) Members() []Leaf {
	var out []Leaf
	for i := uint32(0); i < pg.tree.LeafCount(); i++ {
		n := pg.tree.GetLeaf(treesync.LeafIndex(i))
		if n == nil || n.LeafData == nil || n.LeafData.Credential == nil {
			continue
		}
		out = append(out, Leaf{Index: i, Identity: string(n.LeafData.Credential.Identity), SignatureKey: append([]byte(nil), n.LeafData.SigKeyBytes()...)})
	}
	return out
}

// FromGroupInfo seeds public state from an MLSMessage-framed GroupInfo that
// carries the ratchet tree, after checking the tree is the one the context
// names, every link of it holds, every leaf signed itself, and a leaf of it
// signed the GroupInfo.
func FromGroupInfo(wire []byte) (*PublicGroup, error) {
	gi, err := parseGroupInfo(wire)
	if err != nil {
		return nil, err
	}
	gc := gi.GroupContext
	var tree *treesync.RatchetTree
	for _, ext := range gi.Extensions {
		if ext.Type == extensions.ExtensionTypeRatchetTree {
			if tree, err = treesync.UnmarshalTreeFromExtension(ext.Data, gc.CipherSuite); err != nil {
				return nil, fmt.Errorf("%w: ratchet tree: %v", ErrUnverified, err)
			}
		}
	}
	if tree == nil {
		return nil, fmt.Errorf("%w: group info carries no ratchet tree", ErrUnverified)
	}
	tree.SetCipherSuite(gc.CipherSuite)
	tree = tree.ExpandToPowerOf2()
	pg := &PublicGroup{cs: gc.CipherSuite, context: gc, tree: tree}
	if !bytes.Equal(tree.TreeHash(), gc.TreeHash) {
		return nil, fmt.Errorf("%w: the tree is not the one the group context names", ErrUnverified)
	}
	if err := tree.VerifyAllParentHashes(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	for i := uint32(0); i < tree.LeafCount(); i++ {
		if n := tree.GetLeaf(treesync.LeafIndex(i)); n != nil && n.LeafData != nil {
			if err := n.LeafData.VerifyWithContext(pg.cs, pg.GroupID(), i); err != nil {
				return nil, fmt.Errorf("%w: leaf %d: %v", ErrUnverified, i, err)
			}
		}
	}
	if err := pg.verifyGroupInfo(gi); err != nil {
		return nil, err
	}
	pg.interim = (&framing.InterimTranscriptHashInput{ConfirmationTag: gi.ConfirmationTag}).Compute(pg.cs, gc.ConfirmedTranscriptHash)
	return pg, nil
}

func parseGroupInfo(wire []byte) (*group.Info, error) {
	raw, err := UnwrapGroupInfo(wire)
	if err != nil {
		return nil, err
	}
	gi, err := group.UnmarshalGroupInfo(raw)
	if err != nil || gi.GroupContext == nil || gi.GroupContext.GroupID == nil {
		return nil, fmt.Errorf("%w: unreadable group info", ErrUnverified)
	}
	return gi, nil
}

// verifyGroupInfo checks a GroupInfo's signature by its signer leaf here.
func (pg *PublicGroup) verifyGroupInfo(gi *group.Info) error {
	signer := pg.tree.GetLeaf(treesync.LeafIndex(gi.Signer))
	if signer == nil || signer.LeafData == nil {
		return fmt.Errorf("%w: group info signer is not a member", ErrUnverified)
	}
	pk := ciphersuite.NewMLSSignaturePublicKey(signer.LeafData.SigKeyBytes(), pg.cs.SignatureScheme())
	if err := ciphersuite.VerifyWithLabel(pk, "GroupInfoTBS", gi.MarshalTBS(), ciphersuite.NewSignature(gi.Signature)); err != nil {
		return fmt.Errorf("%w: group info signature: %v", ErrUnverified, err)
	}
	return nil
}

// CheckGroupInfo verifies a GroupInfo published for this state's epoch: its
// GroupContext is exactly the hub's, a member signed it, and the ratchet
// tree it carries, if any, is this one.
func (pg *PublicGroup) CheckGroupInfo(wire []byte) error {
	gi, err := parseGroupInfo(wire)
	if err != nil {
		return err
	}
	if !bytes.Equal(gi.GroupContext.Marshal(), pg.context.Marshal()) {
		return fmt.Errorf("%w: group info is for another group, epoch or state", ErrUnverified)
	}
	for _, ext := range gi.Extensions {
		if ext.Type == extensions.ExtensionTypeRatchetTree {
			t, err := treesync.UnmarshalTreeFromExtension(ext.Data, pg.cs)
			if err != nil {
				return fmt.Errorf("%w: ratchet tree: %v", ErrUnverified, err)
			}
			t.SetCipherSuite(pg.cs)
			if !bytes.Equal(t.ExpandToPowerOf2().TreeHash(), pg.context.TreeHash) {
				return fmt.Errorf("%w: group info carries another tree", ErrUnverified)
			}
		}
	}
	return pg.verifyGroupInfo(gi)
}

// Next verifies a commit (an MLSMessage-framed PublicMessage) against this
// state and returns the state after it. The receiver is not changed.
func (pg *PublicGroup) Next(wire []byte) (*PublicGroup, error) {
	mm, err := framing.UnmarshalMLSMessage(wire)
	if err != nil {
		return nil, fmt.Errorf("mls: %w", err)
	}
	pm, ok := mm.AsPublic()
	if !ok || pm.Content.ContentType() != framing.ContentTypeCommit {
		return nil, ErrNotPublic
	}
	fc := pm.Content
	if !bytes.Equal(fc.GroupID, pg.GroupID()) || fc.Epoch != pg.Epoch() {
		return nil, fmt.Errorf("%w: commit is for another group or epoch", ErrUnverified)
	}
	data, _ := fc.CommitData()
	commit, err := group.UnmarshalCommit(data)
	if err != nil {
		return nil, fmt.Errorf("mls: commit: %w", err)
	}
	if pm.Auth.Signature == nil {
		return nil, fmt.Errorf("%w: unsigned commit", ErrUnverified)
	}
	ac := &framing.AuthenticatedContent{WireFormat: framing.WireFormatPublicMessage, Content: fc, Auth: pm.Auth, GroupContext: pg.context.Marshal()}

	// Who signed it: the sender's leaf, or an external joiner's path leaf.
	var signer *treesync.LeafNodeData
	external := false
	switch fc.Sender.Type {
	case framing.SenderTypeMember:
		n := pg.tree.GetLeaf(treesync.LeafIndex(fc.Sender.LeafIndex))
		if n == nil || n.LeafData == nil {
			return nil, ErrBadSender
		}
		signer = n.LeafData
	case framing.SenderTypeNewMemberCommit:
		if commit.Path == nil || commit.Path.LeafNode == nil {
			return nil, ErrNoPathLeaf
		}
		signer, external = commit.Path.LeafNode, true
	default:
		return nil, fmt.Errorf("%w: sender type %d", ErrUnverified, fc.Sender.Type)
	}
	pk := ciphersuite.NewMLSSignaturePublicKey(signer.SigKeyBytes(), pg.cs.SignatureScheme())
	if err := ciphersuite.VerifyWithLabel(pk, "FramedContentTBS", ac.MarshalTBS(), pm.Auth.Signature); err != nil {
		return nil, fmt.Errorf("%w: commit signature: %v", ErrUnverified, err)
	}

	// The tree after its proposals, in RFC 9420 §12.3 order.
	tree := pg.tree.Clone()
	tree.SetCipherSuite(pg.cs)
	exts := pg.context.Extensions
	var props []*group.Proposal
	for _, por := range commit.Proposals {
		if por.Proposal == nil {
			return nil, ErrUnsupported
		}
		props = append(props, por.Proposal)
	}
	for _, p := range props {
		if p.Type == group.ProposalTypeGroupContextExtensions && p.GroupContextExtensions != nil {
			exts = p.GroupContextExtensions.Extensions
			if exts == nil {
				exts = []group.Extension{}
			}
		}
	}
	for _, p := range props {
		if p.Type == group.ProposalTypeRemove && p.Remove != nil {
			if n := tree.GetLeaf(treesync.LeafIndex(p.Remove.Removed)); n == nil || n.LeafData == nil {
				return nil, ErrBlankLeaf
			}
			for _, idx := range tree.DirectPath(treesync.LeafIndex(p.Remove.Removed)) {
				tree.BlankNode(idx)
			}
		}
	}
	for _, p := range props {
		if p.Type != group.ProposalTypeAdd {
			continue
		}
		if p.Add == nil || p.Add.KeyPackage == nil || p.Add.KeyPackage.LeafNode == nil {
			return nil, fmt.Errorf("%w: add without key package", ErrUnverified)
		}
		kp := p.Add.KeyPackage
		if err := kp.Verify(pg.cs); err != nil {
			return nil, fmt.Errorf("%w: key package: %v", ErrUnverified, err)
		}
		leaf, err := leafData(kp.LeafNode.Marshal())
		if err != nil {
			return nil, err
		}
		if err := leaf.Verify(pg.cs); err != nil {
			return nil, fmt.Errorf("%w: added leaf: %v", ErrUnverified, err)
		}
		tree.AddLeaf(*leaf)
	}
	tree.TruncateTrailingBlanks()

	// The committer's path: its leaf and new keys up its direct path.
	committer := treesync.LeafIndex(fc.Sender.LeafIndex)
	if external {
		committer, _ = tree.AddLeaf(*commit.Path.LeafNode)
	}
	needPath := len(props) == 0 || external
	for _, p := range props {
		if p.Type == group.ProposalTypeRemove {
			needPath = true
		}
	}
	if commit.Path == nil {
		if needPath {
			return nil, fmt.Errorf("%w: commit needs a path and has none", ErrUnverified)
		}
		tree = tree.ExpandToPowerOf2()
	} else {
		pl := commit.Path.LeafNode
		if pl == nil || pl.LeafNodeSource != 3 {
			return nil, fmt.Errorf("%w: path leaf is not a commit leaf", ErrUnverified)
		}
		if err := pl.VerifyWithContext(pg.cs, pg.GroupID(), uint32(committer)); err != nil {
			return nil, fmt.Errorf("%w: path leaf: %v", ErrUnverified, err)
		}
		if !external {
			if cur := pg.tree.GetLeaf(committer); cur != nil && cur.LeafData != nil && bytes.Equal(cur.LeafData.EncryptionKey, pl.EncryptionKey) {
				return nil, fmt.Errorf("%w: path leaf keeps its old encryption key", ErrUnverified)
			}
		}
		tree = tree.ExpandToPowerOf2()
		if tree, err = applyPath(tree, committer, commit.Path, pg.cs); err != nil {
			return nil, err
		}
		if err := tree.VerifyAllParentHashes(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnverified, err)
		}
	}

	confirmedIn, err := framing.NewConfirmedTranscriptHashInput(ac)
	if err != nil {
		return nil, err
	}
	confirmed, err := confirmedIn.Compute(pg.cs, pg.interim)
	if err != nil {
		return nil, err
	}
	next := &PublicGroup{cs: pg.cs, tree: tree, context: &group.GroupContext{
		Version: pg.context.Version, CipherSuite: pg.context.CipherSuite, GroupID: pg.context.GroupID,
		Epoch: group.NewGroupEpoch(pg.Epoch() + 1), TreeHash: tree.TreeHash(), ConfirmedTranscriptHash: confirmed, Extensions: exts,
	}}
	next.interim = (&framing.InterimTranscriptHashInput{ConfirmationTag: pm.Auth.ConfirmationTag}).Compute(pg.cs, confirmed)
	return next, nil
}

func leafData(raw []byte) (*treesync.LeafNodeData, error) {
	l, err := treesync.UnmarshalLeafNodeData(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: leaf node: %v", ErrUnverified, err)
	}
	return l, nil
}

// applyPath puts an UpdatePath into the tree as every member does (RFC
// 9420 §12.4.2): the committer's new leaf, its direct path blanked, the
// filtered direct path's new keys, and parent hashes recomputed top-down.
// The UpdatePath must name a key for exactly the filtered levels.
func applyPath(tree *treesync.RatchetTree, committer treesync.LeafIndex, path *group.UpdatePath, cs ciphersuite.CipherSuite) (*treesync.RatchetTree, error) {
	t := tree.Clone()
	t.SetCipherSuite(cs)
	if err := t.SetLeaf(committer, *path.LeafNode); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	dp := t.DirectPath(committer)
	for i := 1; i < len(dp); i++ {
		t.BlankNode(dp[i])
	}
	copath := t.Copath(committer)
	var levels []int
	for i, c := range copath {
		if len(t.Resolution(c)) > 0 {
			levels = append(levels, i)
		}
	}
	if len(levels) != len(path.Nodes) {
		return nil, fmt.Errorf("%w: path has %d nodes for %d filtered levels", ErrUnverified, len(path.Nodes), len(levels))
	}
	for m, level := range levels {
		if level+1 >= len(dp) {
			return nil, fmt.Errorf("%w: path longer than the tree", ErrUnverified)
		}
		key, err := cs.Curve().NewPublicKey(path.Nodes[m].EncryptionKey)
		if err != nil {
			return nil, fmt.Errorf("%w: path key: %v", ErrUnverified, err)
		}
		n := &t.Nodes[dp[level+1]]
		n.EncryptionKey, n.State, n.UnmergedLeaves = key, treesync.NodeStatePresent, nil
	}
	if len(dp) > 1 {
		t.Nodes[t.Root()].ParentHash = []byte{}
		for k := len(dp) - 2; k >= 0; k-- {
			child, parent := dp[k], &t.Nodes[dp[k+1]]
			ph := parent.ParentHash
			if parent.State == treesync.NodeStatePresent {
				var pk []byte
				if parent.EncryptionKey != nil {
					pk = parent.EncryptionKey.Bytes()
				}
				ph = treesync.ComputeParentHash(pk, parent.ParentHash, t.HashNode(t.GetSibling(child)), cs.HashFunction())
			}
			if !treesync.IsLeaf(child) {
				t.Nodes[child].ParentHash = ph
			}
		}
	}
	return t, nil
}
