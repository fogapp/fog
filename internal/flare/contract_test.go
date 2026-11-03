package flare

import (
	"os"
	"strings"
	"testing"
)

// The cross-language contract: testdata/ruby-envelope.txt is a byte-for-byte
// envelope produced by the flare gem's own builder and sender code
// (clients/flare; regenerate with a script that mirrors HttpSender). If the
// gem and this parser ever drift, this fails before an app in production
// finds out.
func TestRubyGemEnvelopeContract(t *testing.T) {
	f, err := os.Open("testdata/ruby-envelope.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	env, err := ParseEnvelope(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Items) != 1 || env.Items[0].Type != "event" {
		t.Fatalf("items: %+v", env.Items)
	}
	if dsn, _ := env.Header["dsn"].(string); dsn == "" {
		t.Fatal("gem envelopes carry the dsn in the header (one of the auth paths)")
	}

	ev, err := ParseEvent(env.Items[0].Payload)
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev.Platform != "ruby" || ev.EventID == "" {
		t.Fatalf("identity: %+v", ev)
	}
	// The chain the gem built: ArgumentError (root cause) wrapped by
	// RuntimeError — the class that names the group is the wrapper.
	if got := ev.ErrorClass(); got != "RuntimeError" {
		t.Fatalf("class = %s", got)
	}
	if len(ev.Exception.Values) != 2 || ev.Exception.Values[0].Type != "ArgumentError" {
		t.Fatalf("chain: %+v", ev.Exception.Values)
	}
	if !ev.Unhandled() {
		t.Fatal("handled:false from the gem must read as unhandled")
	}
	if ev.Transaction != "SyncFeedJob" {
		t.Fatalf("transaction = %s", ev.Transaction)
	}
	// The gem's in_app frames must give the fingerprinter something to work
	// with — and volatile message content ("10.0.0.7") must not leak in.
	crash := ev.Crash()
	if crash.Stacktrace == nil || len(inAppFrames(crash.Stacktrace.Frames)) == 0 {
		t.Fatal("expected in-app frames from the gem")
	}
	if fp := Fingerprint(ev); fp == "" {
		t.Fatal("fingerprint should compute")
	}
	// The gem scrubbed context: password was filtered before it ever left
	// the app. The raw payload is the proof.
	raw := string(env.Items[0].Payload)
	if !strings.Contains(raw, `"password":"[FILTERED]"`) {
		t.Fatal("expected [FILTERED] password in extra")
	}
	if strings.Contains(raw, "sekrit") {
		t.Fatal("the secret value must not appear anywhere in the payload")
	}
}
