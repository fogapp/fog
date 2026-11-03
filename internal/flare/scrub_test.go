package flare

import (
	"encoding/json"
	"strings"
	"testing"
)

const richEvent = `{
	"event_id": "aa11", "timestamp": "2026-08-13T00:00:00Z", "platform": "ruby",
	"level": "error", "transaction": "PostsController#show",
	"release": "abc123", "environment": "production", "server_name": "davebox",
	"message": "boom",
	"exception": {"values": [{
		"type": "RuntimeError", "value": "boom",
		"mechanism": {"type": "rails", "handled": false},
		"stacktrace": {"frames": [{
			"filename": "app/models/post.rb", "function": "lookup", "lineno": 4,
			"in_app": true, "context_line": "    Post.find!(slug)",
			"vars": {"slug": "daves-private-note-title"}
		}]}
	}]},
	"request": {"url": "https://manor.dave.example/posts/42", "method": "GET",
		"data": {"q": "daves search"}},
	"user": {"id": "dave", "email": "dave@example.com"},
	"breadcrumbs": {"values": [{"message": "clicked a private thing"}]},
	"tags": {"household": "dave-family"},
	"extra": {"note": "private context"}
}`

func TestRedactEventKeepsCodeDropsUsage(t *testing.T) {
	out, err := RedactEvent([]byte(richEvent))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	// What survives: everything needed to fix the bug.
	for _, want := range []string{"RuntimeError", "app/models/post.rb", "Post.find!", "abc123", "PostsController#show", `"handled":false`} {
		if !strings.Contains(s, want) {
			t.Errorf("redacted payload lost %q", want)
		}
	}
	// What must not: anything about how the instance is used. Frame-local
	// variables go too — they hold runtime values, not code.
	for _, banned := range []string{"dave@example.com", "daves search", "private thing", "dave-family", "private context", "daves-private-note-title", "manor.dave.example"} {
		if strings.Contains(s, banned) {
			t.Errorf("redacted payload leaked %q", banned)
		}
	}

	// It must still parse and group like an event.
	ev, err := ParseEvent(out)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ErrorClass() != "RuntimeError" || Fingerprint(ev) == "" {
		t.Fatalf("redacted event degraded: %+v", ev)
	}
	if !ev.Unhandled() {
		t.Fatal("redaction must not erase the crash marker")
	}
}

func TestSetOriginTag(t *testing.T) {
	out, err := RedactEvent([]byte(richEvent))
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := SetOriginTag(out, "manor")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := ParseEvent(tagged)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Tags["fog.app"] != "manor" {
		t.Fatalf("tags: %+v", ev.Tags)
	}
	// Redaction dropped the peer's own tags before the origin was stamped.
	if _, ok := ev.Tags["household"]; ok {
		t.Fatal("origin stamping must not resurrect dropped tags")
	}
}

func TestTagMapEncodings(t *testing.T) {
	var obj struct {
		Tags TagMap `json:"tags"`
	}
	if err := json.Unmarshal([]byte(`{"tags":{"a":"x","n":3}}`), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Tags["a"] != "x" || obj.Tags["n"] != "3" {
		t.Fatalf("object tags: %+v", obj.Tags)
	}
	var arr struct {
		Tags TagMap `json:"tags"`
	}
	if err := json.Unmarshal([]byte(`{"tags":[["a","x"]]}`), &arr); err != nil {
		t.Fatal(err)
	}
	if arr.Tags["a"] != "x" {
		t.Fatalf("pair tags: %+v", arr.Tags)
	}
}
