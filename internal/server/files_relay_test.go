package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// fogNode is one live fog for multi-household tests.
type fogNode struct {
	s      *Server
	h      http.Handler
	cookie *http.Cookie
	http   *httptest.Server
}

func newFogNode(t *testing.T) *fogNode {
	t.Helper()
	n := &fogNode{}
	n.s, n.h = newTestServer(t)
	n.cookie = setup(t, n.h)
	n.http = httptest.NewServer(n.h)
	t.Cleanup(n.http.Close)
	if w := do(t, n.h, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":%q}`, n.http.URL), withCookie(n.cookie)); w.Code != http.StatusOK {
		t.Fatalf("set url: %d %s", w.Code, w.Body)
	}
	return n
}

// pairNodes pairs joiner with inviter and returns each one's row for the
// other: inviter's row for joiner, joiner's row for inviter.
func pairNodes(t *testing.T, inviter, joiner *fogNode, grants string) (joinerOnInviter, inviterOnJoiner *store.FedPeer) {
	t.Helper()
	code := mintInvite(t, inviter.h, inviter.cookie, grants)
	if w := do(t, joiner.h, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(joiner.cookie)); w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	joinerOnInviter, _ = inviter.s.store.FedPeerByInstanceID(mustIdentity(t, joiner.s).InstanceID)
	inviterOnJoiner, _ = joiner.s.store.FedPeerByInstanceID(mustIdentity(t, inviter.s).InstanceID)
	return
}

// A contribution's bytes go from its author to another subscriber of the
// origin only against the origin's ticket: issued for that subscriber, for
// that file, while the subscriber's own share carries it with its bytes.
// The author's grant to the subscriber is not enough on its own.
func TestContributionRelayIsGatedByTheOrigin(t *testing.T) {
	origin, author, holder := newFogNode(t), newFogNode(t), newFogNode(t)
	authorOnOrigin, originOnAuthor := pairNodes(t, origin, author, `[{"capability":"files.share"}]`)
	holderOnOrigin, originOnHolder := pairNodes(t, origin, holder, `[{"capability":"files.share"}]`)
	_, authorOnHolder := pairNodes(t, author, holder, `[{"capability":"files.share"}]`)
	ctx := context.Background()
	oID, aID := mustIdentity(t, origin.s), mustIdentity(t, author.s)

	// The origin holds "Drop", contributed to by the author, shared full
	// with the holder.
	os.MkdirAll(filepath.Join(origin.s.files.Root(), "Drop"), 0o755)
	os.WriteFile(filepath.Join(origin.s.files.Root(), "Drop", "x.txt"), []byte("from the author"), 0o644)
	toAuthor, err := origin.s.filesFed.CreateShare(&store.FileFedShare{PeerID: authorOnOrigin.ID, Path: "Drop", Title: "Drop",
		Mode: filesfed.ModeContribute, Scope: filesfed.ScopeAll, Detail: filesfed.DetailFull, Apps: filesfed.AppsDenied})
	if err != nil {
		t.Fatal(err)
	}
	toHolder, err := origin.s.filesFed.CreateShare(&store.FileFedShare{PeerID: holderOnOrigin.ID, Path: "Drop", Title: "Drop",
		Mode: filesfed.ModeRead, Scope: filesfed.ScopeAll, Detail: filesfed.DetailFull, Apps: filesfed.AppsDenied})
	if err != nil {
		t.Fatal(err)
	}
	origin.s.store.UpsertFileFedOrigin(store.FileFedOrigin{Path: "Drop/x.txt", PeerID: authorOnOrigin.ID, ShareID: toAuthor.ID, InstanceID: aID.InstanceID, Size: 15})

	// The author's side: its contribute subscription and the file it pushed.
	sub, err := author.s.store.CreateFileFedSubscription(&store.FileFedSubscription{PeerID: originOnAuthor.ID, RemoteShareID: toAuthor.ID,
		Title: "Drop", Folder: "Peers/origin/Drop", Mode: filesfed.ModeContribute, DetailOffered: filesfed.DetailFull, DetailWanted: filesfed.DetailFull})
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(author.s.files.Root(), "Peers", "origin", "Drop"), 0o755)
	os.WriteFile(filepath.Join(author.s.files.Root(), "Peers", "origin", "Drop", "x.txt"), []byte("from the author"), 0o644)
	author.s.store.UpsertFileFedReplica(store.FileFedReplica{SubscriptionID: sub.ID, Rel: "x.txt", Kind: "file", Hydrated: true, Origin: aID.InstanceID, PushedAt: 1})

	blob := fmt.Sprintf("/fed/v1/files/contributions/%s/%d/blob/x.txt", oID.InstanceID, toAuthor.ID)
	fetch := func(ticket string) error {
		h := http.Header{}
		if ticket != "" {
			h.Set(filesfed.HeaderRelayTicket, ticket)
		}
		resp, err := holder.s.fed.SignedDoHeaders(ctx, authorOnHolder, "GET", blob, "", nil, h)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if b, _ := io.ReadAll(resp.Body); string(b) != "from the author" {
			return fmt.Errorf("got %q", b)
		}
		return nil
	}
	ticket := func() (string, error) {
		var out struct{ Ticket string }
		_, err := holder.s.fed.SignedGetJSON(ctx, originOnHolder, fmt.Sprintf("/fed/v1/files/shares/%d/relay/x.txt", toHolder.ID), "", &out)
		return out.Ticket, err
	}

	var re *fed.RemoteError
	if err := fetch(""); !errors.As(err, &re) || re.Status != http.StatusForbidden {
		t.Fatalf("without the origin's ticket: %v", err)
	}
	tk, err := ticket()
	if err != nil {
		t.Fatal(err)
	}
	if err := fetch(tk); err != nil {
		t.Fatalf("with the origin's ticket: %v", err)
	}
	// The ticket names its file: it opens nothing else.
	other := fmt.Sprintf("/fed/v1/files/contributions/%s/%d/blob/y.txt", oID.InstanceID, toAuthor.ID)
	if _, err := holder.s.fed.SignedDoHeaders(ctx, authorOnHolder, "GET", other, "", nil, http.Header{filesfed.HeaderRelayTicket: {tk}}); !errors.As(err, &re) || re.Status != http.StatusForbidden {
		t.Fatalf("a ticket for another file: %v", err)
	}

	// The origin narrows the holder's share to a listing: no more tickets.
	toHolder.Detail = filesfed.DetailListing
	if _, err := origin.s.filesFed.UpdateShare(toHolder); err != nil {
		t.Fatal(err)
	}
	if _, err := ticket(); !errors.As(err, &re) || re.Status != http.StatusNotFound {
		t.Fatalf("a ticket through a listing: %v", err)
	}
}
