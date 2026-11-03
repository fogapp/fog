package setup

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHandlerRefusesARebindingHost: a page whose DNS name was rebound to
// this machine's address is same-origin with itself, so the Origin check
// alone passes it; its Host names the attacker's domain, which this machine
// is not.
func TestHandlerRefusesARebindingHost(t *testing.T) {
	a := fastAgent()
	body, _ := json.Marshal(goodCfg())
	for _, path := range []string{"/api/setup/install", "/api/setup/claim-token"} {
		req := newTestRequest(t, "POST", path, strings.NewReader(string(body)), "application/json")
		req.Host = "rebind.evil.example:8080"
		req.Header.Set("Origin", "http://rebind.evil.example:8080")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if res := doHandler(a, req); res.Code != 421 {
			t.Fatalf("%s from a rebound name: %d %s", path, res.Code, res.Body.String())
		}
	}
}
