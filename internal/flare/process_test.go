package flare

import (
	"encoding/json"
	"strings"
	"testing"
)

// An oversized payload whose weight is almost entirely a request body: the
// slim path must drop Request (stored verbatim as json.RawMessage, never
// read by grouping) rather than re-marshal it back in.
func TestSlimPayloadDropsRequest(t *testing.T) {
	bigBody, _ := json.Marshal(strings.Repeat("x", 400<<10))
	raw := `{"event_id":"aa","platform":"ruby",
		"exception":{"values":[{"type":"BoomError","value":"boom",
			"stacktrace":{"frames":[{"filename":"app/a.rb","function":"f","in_app":true}]}}]},
		"request":{"url":"http://x/upload","data":` + string(bigBody) + `}}`
	if len(raw) <= maxStoredPayload {
		t.Fatal("fixture must exceed maxStoredPayload")
	}
	ev := mustEvent(t, raw)

	stored := slimPayload([]byte(raw), ev)
	if len(stored) > maxStoredPayload {
		t.Fatalf("slimmed payload = %d bytes, want <= %d", len(stored), maxStoredPayload)
	}
	back := mustEvent(t, string(stored))
	if back.Request != nil {
		t.Fatal("slimmed payload must not carry the request")
	}
	// The stack trace is what grouping and display need; it survives.
	if back.ErrorClass() != "BoomError" || Fingerprint(back) != Fingerprint(ev) {
		t.Fatalf("slimming changed grouping: class=%s", back.ErrorClass())
	}
	// The original parsed event is untouched (it may still be forwarded).
	if ev.Request == nil {
		t.Fatal("slimPayload must not mutate the caller's event")
	}
}

// A payload oversized even after slimming (the bloat is in a parsed field,
// not the request) degrades to the stub: the bound is a guarantee.
func TestSlimPayloadHardCap(t *testing.T) {
	raw := `{"event_id":"aa","message":"` + strings.Repeat("x", 300<<10) + `"}`
	ev := mustEvent(t, raw)
	stored := slimPayload([]byte(raw), ev)
	if string(stored) != `{"truncated":true}` {
		t.Fatalf("still-oversized slim should degrade to stub, got %d bytes", len(stored))
	}
}

func TestSlimPayloadPassesSmallThrough(t *testing.T) {
	raw := `{"event_id":"aa","request":{"url":"http://x"}}`
	ev := mustEvent(t, raw)
	if got := slimPayload([]byte(raw), ev); string(got) != raw {
		t.Fatalf("small payload must be stored verbatim, got %s", got)
	}
}

// The flood guard caps pushes per throttle key: an app's eleventh new group
// in an hour doesn't buzz, while another key (another app, or a peer's
// "peer:<name>") still has its own budget.
func TestAllowPushCapsPerThrottleKey(t *testing.T) {
	p := &Processor{}
	for i := 0; i < pushPerWindow; i++ {
		if !p.allowPush("fit") {
			t.Fatalf("push %d refused under the cap", i+1)
		}
	}
	if p.allowPush("fit") {
		t.Fatal("push over the cap allowed")
	}
	if !p.allowPush("peer:fit") || !p.allowPush("stream") {
		t.Fatal("one key's flood must not silence another's")
	}
}
