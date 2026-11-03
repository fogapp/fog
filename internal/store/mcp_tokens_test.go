package store

import (
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/auth"
)

// The box that upgrades mid-connection: its one settings-held token has to
// keep opening /mcp afterwards, now as a named row, or every connected
// client breaks on a version bump.
func TestSettingsMCPTokenCarriesForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fog.db")
	db := openAtMigration(t, path, "0018")
	token, digest := auth.NewToken()
	if _, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES
		('mcp_token_digest', ?, ?), ('mcp_token_created_at', '2026-01-02T03:04:05Z', ?)`,
		digest, now(), now()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tok, err := s.MCPTokenByDigest(auth.DigestToken(token))
	if err != nil {
		t.Fatalf("pre-upgrade token stopped working: %v", err)
	}
	if tok.Name != "default" || tok.CreatedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("carried-forward token = %+v, want name default and its original createdAt", tok)
	}
	// The settings rows are the old home; leaving them would be a second,
	// unrevocable copy of the credential's digest.
	if v, _ := s.GetSetting("mcp_token_digest"); v != "" {
		t.Error("mcp_token_digest survived the migration")
	}
}

// A fresh box has no tokens at all — the carry-forward must not invent one.
func TestNoMCPTokenWithoutOne(t *testing.T) {
	s := testStore(t)
	tokens, err := s.ListMCPTokens()
	if err != nil || len(tokens) != 0 {
		t.Fatalf("fresh box: %v %+v", err, tokens)
	}
	if _, err := s.MCPTokenByDigest("whatever"); err != ErrNotFound {
		t.Fatalf("unknown digest: got %v, want ErrNotFound", err)
	}
}

// Every lookup path reports the chat flag: the digest and ID paths once
// scanned their own columns and always said CanPostChat=false.
func TestMCPTokenCanPostChatRoundTrips(t *testing.T) {
	s := testStore(t)
	tok, err := s.CreateMCPToken("poster", "digest-p", true, false)
	if err != nil {
		t.Fatal(err)
	}
	byDigest, err := s.MCPTokenByDigest("digest-p")
	if err != nil || !byDigest.CanPostChat {
		t.Fatalf("by digest: %v %+v", err, byDigest)
	}
	byID, err := s.MCPTokenByID(tok.ID)
	if err != nil || !byID.CanPostChat {
		t.Fatalf("by id: %v %+v", err, byID)
	}
}

// The Drive scope is narrow unless asked for, and every lookup path reads it.
func TestMCPTokenCanReadAllFilesRoundTrips(t *testing.T) {
	s := testStore(t)
	narrow, err := s.CreateMCPToken("narrow", "digest-n", false, false)
	if err != nil || narrow.CanReadAllFiles {
		t.Fatalf("narrow: %v %+v", err, narrow)
	}
	tok, err := s.CreateMCPToken("broad", "digest-b", false, true)
	if err != nil {
		t.Fatal(err)
	}
	byDigest, err := s.MCPTokenByDigest("digest-b")
	if err != nil || !byDigest.CanReadAllFiles || byDigest.CanPostChat {
		t.Fatalf("by digest: %v %+v", err, byDigest)
	}
	byID, err := s.MCPTokenByID(tok.ID)
	if err != nil || !byID.CanReadAllFiles {
		t.Fatalf("by id: %v %+v", err, byID)
	}
	if n, err := s.MCPTokenByDigest("digest-n"); err != nil || n.CanReadAllFiles {
		t.Fatalf("narrow by digest: %v %+v", err, n)
	}
}

func TestMCPTokenLifecycle(t *testing.T) {
	s := testStore(t)

	a, err := s.CreateMCPToken("claude-code", "digest-a", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMCPToken("openclaw", "digest-b", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMCPToken("claude-code", "digest-c", false, false); err != ErrNameTaken {
		t.Fatalf("duplicate name: got %v, want ErrNameTaken", err)
	}

	// Use marks the token; the other one stays untouched, so the list says
	// which client is actually connected.
	if _, err := s.MCPTokenByDigest("digest-a"); err != nil {
		t.Fatal(err)
	}
	tokens, err := s.ListMCPTokens()
	if err != nil || len(tokens) != 2 {
		t.Fatalf("list: %v %+v", err, tokens)
	}
	for _, tok := range tokens {
		used := tok.LastUsedAt != ""
		if want := tok.Name == "claude-code"; used != want {
			t.Errorf("%s lastUsedAt=%q, used=%v want %v", tok.Name, tok.LastUsedAt, used, want)
		}
	}

	// By-ID lookup names the row (the revoke path audits by name) without
	// touching last_used_at the way the digest path does.
	if tok, err := s.MCPTokenByID(a.ID); err != nil || tok.Name != "claude-code" {
		t.Fatalf("MCPTokenByID: %v %+v", err, tok)
	}
	if _, err := s.MCPTokenByID(a.ID + 999); err != ErrNotFound {
		t.Fatalf("MCPTokenByID unknown id: got %v, want ErrNotFound", err)
	}

	if err := s.DeleteMCPToken(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MCPTokenByDigest("digest-a"); err != ErrNotFound {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
	if _, err := s.MCPTokenByDigest("digest-b"); err != nil {
		t.Fatalf("other token after delete: %v", err)
	}
	if err := s.DeleteMCPToken(a.ID); err != ErrNotFound {
		t.Fatalf("delete twice: got %v, want ErrNotFound", err)
	}
	// The name is free again once its token is gone.
	if _, err := s.CreateMCPToken("claude-code", "digest-d", false, false); err != nil {
		t.Fatalf("reusing a revoked name: %v", err)
	}
}
