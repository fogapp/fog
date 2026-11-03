package server

import (
	"net/http"
	"testing"
)

// A person's ntfy topic, and the operator's, are one path segment on the
// household's ntfy server. One carrying a query or an encoded slash would
// aim fog's POST at something else there (ntfy acts on ?email= and ?call=),
// so both are refused when saved, not only when sent.
func TestNtfyTopicsMustBeOneSegment(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)
	for _, topic := range []string{"me?email=victim@example.com", "a%2Fb", "..", "me#x", "dots.are.not.ntfy"} {
		body := `{"ntfyTopic":"` + topic + `"}`
		if w := do(t, h, "PUT", "/api/chat/me/prefs", body, withCookie(c)); w.Code != http.StatusBadRequest {
			t.Errorf("personal topic %q: got %d %s, want 400", topic, w.Code, w.Body)
		}
		if w := do(t, h, "PUT", "/api/settings", `{"ntfy_topic":"`+topic+`"}`, withCookie(c)); w.Code != http.StatusBadRequest {
			t.Errorf("operator topic %q: got %d %s, want 400", topic, w.Code, w.Body)
		}
	}
	if w := do(t, h, "PUT", "/api/chat/me/prefs", `{"ntfyTopic":"kim-phone_2"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Errorf("plain personal topic: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/settings", `{"ntfy_topic":"fog-alerts"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Errorf("plain operator topic: %d %s", w.Code, w.Body)
	}
	for _, server := range []string{"https://ntfy.sh/?email=x@y", "httpfoo", "https://"} {
		if w := do(t, h, "PUT", "/api/settings", `{"ntfy_server":"`+server+`"}`, withCookie(c)); w.Code != http.StatusBadRequest {
			t.Errorf("server %q: got %d, want 400", server, w.Code)
		}
	}
}
