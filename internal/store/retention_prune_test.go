package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestPruneChatBotSeenForgetsOnlyDismissedNotices(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, kind := range []string{"keep", "drop"} {
		if err := s.CreateNotice(kind, "", kind, ""); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := s.ActiveNotices()
	if err != nil || len(notices) != 2 {
		t.Fatalf("notices: %v, %v", notices, err)
	}
	ids := map[string]int64{}
	for _, n := range notices {
		ids[n.Kind] = n.ID
		if err := s.MarkChatBotSeen("notice", fmt.Sprint(n.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DismissNotice(ids["drop"]); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneChatBotSeen()
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want 1", n, err)
	}
	if seen, _ := s.ChatBotSeen("notice", fmt.Sprint(ids["keep"])); !seen {
		t.Error("active notice's mark was pruned — the bot would post it again")
	}
	if seen, _ := s.ChatBotSeen("notice", fmt.Sprint(ids["drop"])); seen {
		t.Error("dismissed notice's mark survived")
	}
}
