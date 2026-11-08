package store

import (
	"testing"
	"time"
)

// The bot's plaintext of a message is pruned with the message: by room
// retention, and when a disappearing message expires.
func TestChatPruneTakesBotPlaintext(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if err := s.CreateChatRoom(&ChatRoom{ID: "r1", GroupID: []byte("g1"), Kind: "group", Label: "r", Policy: "{}", ReplicaStatus: "live"}); err != nil {
		t.Fatal(err)
	}
	ts := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	old, recent := ts(-72*time.Hour), ts(-time.Hour)
	event := func(seq int64, hubAt string, expires any) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO chat_events (room_id, seq, id, epoch, kind, sender_addr, sender_device, sent_at, hub_at, payload, hints, expires_at)
			VALUES ('r1', ?, ?, 1, 'mls_private', 'a', 0, ?, ?, x'00', '', ?)`, seq, "e"+string(rune('0'+seq)), hubAt, hubAt, expires); err != nil {
			t.Fatal(err)
		}
		if err := s.PutBotMessage(&BotMessage{RoomID: "r1", Seq: seq, ID: "m", Sender: "a", At: hubAt, Text: "secret"}); err != nil {
			t.Fatal(err)
		}
	}
	event(1, old, nil)                 // aged out by retention
	event(2, recent, ts(-time.Minute)) // disappeared
	event(3, recent, nil)              // kept
	// A bot copy whose event is already gone, older than the cutoff.
	if err := s.PutBotMessage(&BotMessage{RoomID: "r1", Seq: 0, ID: "m", Sender: "a", At: old, Text: "orphan"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.PruneExpiredChatEvents(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneChatEvents("r1", ts(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.BotMessages("r1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Seq != 3 {
		t.Fatalf("bot messages after prune: %+v", msgs)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM chat_events WHERE room_id = 'r1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("events after prune: %d", n)
	}
}
