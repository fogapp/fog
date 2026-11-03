package flare

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// A realistic envelope in the shape sentry-ruby sends: header with dsn,
// length-prefixed event item. The length is computed, keeping the fixture
// honest.
func lengthPrefixed(header, itemType, payload string) string {
	return header + "\n" +
		`{"type":"` + itemType + `","length":` + strconv.Itoa(len(payload)) + `}` + "\n" +
		payload + "\n"
}

func TestParseEnvelopeLengthPrefixed(t *testing.T) {
	payload := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","platform":"ruby"}`
	raw := lengthPrefixed(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","dsn":"http://abc123@fog:8080/ingest/4"}`, "event", payload)

	env, err := ParseEnvelope(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 1 || env.Items[0].Type != "event" {
		t.Fatalf("items: %+v", env.Items)
	}
	if string(env.Items[0].Payload) != payload {
		t.Fatalf("payload: %s", env.Items[0].Payload)
	}
	if env.Header["dsn"] != "http://abc123@fog:8080/ingest/4" {
		t.Fatalf("header dsn: %v", env.Header["dsn"])
	}
}

func TestParseEnvelopeNewlineDelimited(t *testing.T) {
	// No length in the item header: payload runs to the next newline. Also
	// checks multiple items and that unknown types are preserved.
	raw := `{}
{"type":"event"}
{"event_id":"aa","platform":"ruby"}
{"type":"client_report"}
{"discarded_events":[]}`

	env, err := ParseEnvelope(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(env.Items))
	}
	if env.Items[0].Type != "event" || env.Items[1].Type != "client_report" {
		t.Fatalf("types: %s, %s", env.Items[0].Type, env.Items[1].Type)
	}
	if string(env.Items[1].Payload) != `{"discarded_events":[]}` {
		t.Fatalf("unterminated final payload: %s", env.Items[1].Payload)
	}
}

func TestParseEnvelopeRejectsGarbage(t *testing.T) {
	for _, raw := range []string{
		"",                          // no header
		"not json\n",                // bad header
		"{}\n{\"length\":5}\nabcde", // item without type
	} {
		if _, err := ParseEnvelope(strings.NewReader(raw)); err == nil {
			t.Errorf("no error for %q", raw)
		}
	}
}

func TestParseEnvelopeBoundsLength(t *testing.T) {
	raw := "{}\n" + `{"type":"event","length":99999999}` + "\nx"
	if _, err := ParseEnvelope(strings.NewReader(raw)); err == nil {
		t.Fatal("oversized declared length must be rejected")
	}
}

func TestExtractKey(t *testing.T) {
	// Header wins.
	h := http.Header{}
	h.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_client=sentry-ruby/5.19, sentry_key=headerkey")
	if k := ExtractKey(h, url.Values{"sentry_key": {"querykey"}}, nil); k != "headerkey" {
		t.Fatalf("header key: %q", k)
	}
	// Then query.
	if k := ExtractKey(http.Header{}, url.Values{"sentry_key": {"querykey"}}, nil); k != "querykey" {
		t.Fatalf("query key: %q", k)
	}
	// Then the envelope header's dsn.
	env := map[string]any{"dsn": "http://dsnkey@fog:8080/ingest/4"}
	if k := ExtractKey(http.Header{}, url.Values{}, env); k != "dsnkey" {
		t.Fatalf("dsn key: %q", k)
	}
	// Nothing anywhere.
	if k := ExtractKey(http.Header{}, url.Values{}, nil); k != "" {
		t.Fatalf("expected empty, got %q", k)
	}
}
