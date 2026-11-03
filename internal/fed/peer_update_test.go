package fed

import (
	"errors"
	"log/slog"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func TestUpdatePeerIsAllOrNothing(t *testing.T) {
	st, vlt := testDeps(t)
	svc := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}
	a, err := st.CreatePairedPeer("inst-a", "alpha", "http://a.lan", "pk-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePairedPeer("inst-b", "beta", "", "pk-b"); err != nil {
		t.Fatal(err)
	}
	rules := CapabilityRules{Known: func(c string) bool { return c == store.CapAppsDirectory }}
	label, url := "Alpha's", "http://moved.lan"

	// Refused before writing: an unknown capability alongside good edits.
	_, err = svc.UpdatePeer(a, PeerUpdate{Label: &label, URL: &url, Capabilities: []CapabilityChange{
		{Capability: "nope", Direction: store.DirUse, Enabled: true},
	}}, rules)
	var invalid *InvalidUpdateError
	if !errors.As(err, &invalid) {
		t.Fatalf("unknown capability: %v", err)
	}

	// Failed while writing: the rename collides, after the label, URL and
	// capability statements already ran inside the transaction.
	_, err = svc.UpdatePeer(a, PeerUpdate{Name: "beta", Label: &label, URL: &url, Capabilities: []CapabilityChange{
		{Capability: store.CapAppsDirectory, Direction: store.DirUse, Enabled: true},
	}}, rules)
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("colliding rename: %v", err)
	}
	got, _ := st.FedPeerByID(a.ID)
	if got.Name != "alpha" || got.Label != "" || got.URL != "http://a.lan" {
		t.Fatalf("partial write survived: %+v", got)
	}
	if _, ok, _ := st.FedCapabilityConfig(a.ID, store.CapAppsDirectory, store.DirUse); ok {
		t.Fatal("capability written by a failed update")
	}

	res, err := svc.UpdatePeer(a, PeerUpdate{Name: "Alpha Two", Label: &label, URL: &url, Capabilities: []CapabilityChange{
		{Capability: store.CapAppsDirectory, Direction: store.DirUse, Enabled: true},
	}}, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Renamed || !res.LabelChanged || !res.URLChanged || len(res.Capabilities) != 1 {
		t.Fatalf("result: %+v", res)
	}
	if res.Peer.Name != "alpha-two" || res.Peer.Label != label || res.Peer.URL != url {
		t.Fatalf("peer: %+v", res.Peer)
	}
}
