package flare

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustEvent(t *testing.T, raw string) *Event {
	t.Helper()
	ev, err := ParseEvent([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// Two events that should group together must fingerprint identically; the
// table pairs each case with a mutation that must NOT change the result and
// one that must.
func TestFingerprintStability(t *testing.T) {
	base := `{
		"event_id": "aa", "platform": "ruby",
		"exception": {"values": [{
			"type": "PG::UndefinedColumn",
			"value": "column posts.slug does not exist",
			"stacktrace": {"frames": [
				{"filename": "app/controllers/application_controller.rb", "function": "run", "lineno": 10, "in_app": true},
				{"filename": "activerecord/lib/connection.rb", "function": "execute", "lineno": 500, "in_app": false},
				{"filename": "app/models/post.rb", "function": "find_by_slug!", "lineno": 42, "in_app": true}
			]}
		}]}
	}`

	// Line numbers churn every deploy — they must not affect grouping.
	differentLines := `{
		"event_id": "bb", "platform": "ruby",
		"exception": {"values": [{
			"type": "PG::UndefinedColumn",
			"value": "column posts.slug does not exist",
			"stacktrace": {"frames": [
				{"filename": "app/controllers/application_controller.rb", "function": "run", "lineno": 11, "in_app": true},
				{"filename": "activerecord/lib/connection.rb", "function": "execute", "lineno": 999, "in_app": false},
				{"filename": "app/models/post.rb", "function": "find_by_slug!", "lineno": 57, "in_app": true}
			]}
		}]}
	}`

	// Gem-internal frames must not affect grouping either.
	differentGemFrames := `{
		"event_id": "cc", "platform": "ruby",
		"exception": {"values": [{
			"type": "PG::UndefinedColumn",
			"value": "column posts.slug does not exist",
			"stacktrace": {"frames": [
				{"filename": "app/controllers/application_controller.rb", "function": "run", "lineno": 10, "in_app": true},
				{"filename": "rack/lib/something_else.rb", "function": "call", "lineno": 1, "in_app": false},
				{"filename": "app/models/post.rb", "function": "find_by_slug!", "lineno": 42, "in_app": true}
			]}
		}]}
	}`

	// A different in-app call path IS a different group.
	differentPath := `{
		"event_id": "dd", "platform": "ruby",
		"exception": {"values": [{
			"type": "PG::UndefinedColumn",
			"value": "column posts.slug does not exist",
			"stacktrace": {"frames": [
				{"filename": "app/jobs/sync_job.rb", "function": "perform", "lineno": 5, "in_app": true},
				{"filename": "app/models/post.rb", "function": "find_by_slug!", "lineno": 42, "in_app": true}
			]}
		}]}
	}`

	fp := Fingerprint(mustEvent(t, base))
	if got := Fingerprint(mustEvent(t, differentLines)); got != fp {
		t.Error("line numbers changed the fingerprint")
	}
	if got := Fingerprint(mustEvent(t, differentGemFrames)); got != fp {
		t.Error("gem frames changed the fingerprint")
	}
	if got := Fingerprint(mustEvent(t, differentPath)); got == fp {
		t.Error("a different in-app path should be a different group")
	}
}

func TestFingerprintExplicit(t *testing.T) {
	ev := mustEvent(t, `{"event_id":"aa","fingerprint":["my-custom-group"],
		"exception":{"values":[{"type":"A","value":"x"}]}}`)
	other := mustEvent(t, `{"event_id":"bb","fingerprint":["my-custom-group"],
		"exception":{"values":[{"type":"B","value":"totally different"}]}}`)
	if Fingerprint(ev) != Fingerprint(other) {
		t.Error("explicit fingerprints must group regardless of content")
	}

	// {{ default }} splices the computed grouping in: same default, different
	// custom dimension → different groups.
	a := mustEvent(t, `{"event_id":"aa","fingerprint":["{{ default }}","tenant-1"],
		"exception":{"values":[{"type":"A","value":"x"}]}}`)
	b := mustEvent(t, `{"event_id":"bb","fingerprint":["{{ default }}","tenant-2"],
		"exception":{"values":[{"type":"A","value":"x"}]}}`)
	if Fingerprint(a) == Fingerprint(b) {
		t.Error("{{ default }} plus differing dimensions should split groups")
	}
}

func TestFingerprintMessageTemplating(t *testing.T) {
	// No stacktrace: volatile values in the message must template away.
	a := mustEvent(t, `{"event_id":"aa","exception":{"values":[{"type":"Timeout","value":"connect to 10.0.3.7:5432 timed out after 30s"}]}}`)
	b := mustEvent(t, `{"event_id":"bb","exception":{"values":[{"type":"Timeout","value":"connect to 10.0.3.9:5432 timed out after 31s"}]}}`)
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("interpolated numbers should not split groups")
	}
	c := mustEvent(t, `{"event_id":"cc","exception":{"values":[{"type":"KeyError","value":"key not found: \"user_id\""}]}}`)
	d := mustEvent(t, `{"event_id":"dd","exception":{"values":[{"type":"KeyError","value":"key not found: \"account\""}]}}`)
	if Fingerprint(c) != Fingerprint(d) {
		t.Error("quoted values should not split groups")
	}
}

func TestNormalizeMessage(t *testing.T) {
	cases := map[string]string{
		`connect to 10.0.3.7:5432 failed`: "connect to ?.?.?.?:? failed",
		`key not found: "user_id"`:        "key not found: ?",
		`job 4f3a2b1c9d8e7f60 crashed`:    "job ? crashed",
		`plain message`:                   "plain message",
	}
	for in, want := range cases {
		if got := NormalizeMessage(in); got != want {
			t.Errorf("NormalizeMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEventDerivations(t *testing.T) {
	ev := mustEvent(t, `{
		"event_id": "aa", "level": "warning", "transaction": "PostsController#show",
		"release": "abc123", "environment": "production",
		"exception": {"values": [
			{"type": "RootCause", "value": "root", "mechanism": {"type": "generic", "handled": true}},
			{"type": "WrapperError", "value": "outer",
			 "mechanism": {"type": "rails", "handled": false},
			 "stacktrace": {"frames": [
				{"filename": "gem/lib/x.rb", "function": "call", "in_app": false},
				{"filename": "app/models/post.rb", "function": "lookup", "in_app": true}
			]}}
		]}
	}`)
	if ev.ErrorClass() != "WrapperError" {
		t.Errorf("class = %s (want the most recently raised)", ev.ErrorClass())
	}
	if ev.Title() != "WrapperError: outer" {
		t.Errorf("title = %s", ev.Title())
	}
	if ev.Culprit() != "PostsController#show" {
		t.Errorf("culprit = %s (transaction should win)", ev.Culprit())
	}
	if !ev.Unhandled() {
		t.Error("handled:false anywhere in the chain means unhandled")
	}
	if ev.NormLevel() != "warning" {
		t.Errorf("level = %s", ev.NormLevel())
	}

	// Message-only event (Rails.error.report with a string).
	msg := mustEvent(t, `{"event_id":"bb","message":"disk almost full"}`)
	if msg.ErrorClass() != "message" || msg.Title() != "disk almost full" {
		t.Errorf("message event: class=%s title=%s", msg.ErrorClass(), msg.Title())
	}
	if msg.Unhandled() {
		t.Error("message events are by definition handled")
	}

	// Culprit falls back to the innermost in-app frame without a transaction.
	noTxn := mustEvent(t, `{"event_id":"cc","exception":{"values":[{
		"type":"E","value":"x","stacktrace":{"frames":[
			{"filename":"app/models/post.rb","function":"lookup","in_app":true},
			{"filename":"gem/lib/x.rb","function":"call","in_app":false}
	]}}]}}`)
	if noTxn.Culprit() != "lookup (app/models/post.rb)" {
		t.Errorf("culprit fallback = %s", noTxn.Culprit())
	}
}

func TestEventMessageEncodings(t *testing.T) {
	for _, raw := range []string{
		`{"event_id":"aa","message":"plain string"}`,
		`{"event_id":"aa","message":{"formatted":"plain string"}}`,
	} {
		ev := mustEvent(t, raw)
		if ev.Message.Formatted != "plain string" {
			t.Errorf("message from %s = %q", raw, ev.Message.Formatted)
		}
	}
	// Bare-array exception encoding.
	ev := mustEvent(t, `{"event_id":"aa","exception":[{"type":"E","value":"x"}]}`)
	if len(ev.Exception.Values) != 1 || ev.Exception.Values[0].Type != "E" {
		t.Errorf("bare-array exception: %+v", ev.Exception.Values)
	}
	// Round-trip: a re-marshaled event (the oversized-payload path) must
	// still parse and fingerprint identically.
	orig := mustEvent(t, `{"event_id":"aa","exception":{"values":[{"type":"E","value":"x",
		"stacktrace":{"frames":[{"filename":"app/a.rb","function":"f","in_app":true}]}}]}}`)
	slim, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	back := mustEvent(t, string(slim))
	if Fingerprint(orig) != Fingerprint(back) {
		t.Error("re-marshaled event changed fingerprint")
	}
}

func TestTitleTruncatesOnRuneBoundary(t *testing.T) {
	// "a" + 200 two-byte runes = 401 bytes, and byte maxTitleLen lands
	// mid-rune: a byte-offset cut would store invalid UTF-8.
	long := "a" + strings.Repeat("é", 200)
	ev := &Event{Message: EventMessage{Formatted: long}}
	title := ev.Title()
	if !utf8.ValidString(title) {
		t.Fatalf("truncated title is invalid UTF-8: %q", title)
	}
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("truncated title should end with ellipsis: %q", title)
	}
	if len(title) > maxTitleLen+len("…") {
		t.Fatalf("title = %d bytes, want <= %d", len(title), maxTitleLen+len("…"))
	}
}
