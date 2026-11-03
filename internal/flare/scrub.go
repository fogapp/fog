package flare

import (
	"encoding/json"
	"fmt"
)

// RedactEvent is the sender-side privacy filter for fog-to-fog forwarding's
// default "redacted" mode. It is a whitelist, not a blacklist: only the
// fields needed to fix a bug survive — exception chains with stack traces
// and source context (it's the developer's own code), release, environment,
// transaction, level, timestamps. Everything about how the instance is used
// — request data, user identity, breadcrumbs, custom context, tags — is
// dropped. New fields an SDK invents are therefore dropped by default,
// which is the correct failure mode for a privacy filter.
func RedactEvent(payload []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("redact: %w", err)
	}
	out := map[string]json.RawMessage{}
	for _, k := range []string{
		"event_id", "timestamp", "platform", "level", "transaction",
		"release", "environment", "fingerprint", "message",
	} {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	// The exception subtree is kept but re-built through the typed parser,
	// which drops anything it doesn't model — notably frame-local variables
	// ("vars"), which sentry-ruby can capture and which hold runtime values.
	if raw, ok := m["exception"]; ok {
		var chain ExceptionChain
		if err := json.Unmarshal(raw, &chain); err == nil {
			clean, err := json.Marshal(chain)
			if err == nil {
				out["exception"] = clean
			}
		}
	}
	return json.Marshal(out)
}

// SetOriginTag stamps the app name a forwarded event belongs to. Applied
// after redaction (redaction drops tags wholesale), and read back by the
// receiving instance — which trusts the tag only for the app *name*; the
// peer identity always comes from the authenticated token.
func SetOriginTag(payload []byte, appName string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	tags := TagMap{}
	if raw, ok := m["tags"]; ok {
		json.Unmarshal(raw, &tags) // unparseable tags are replaced, not fatal
	}
	tags["fog.app"] = appName
	enc, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	m["tags"] = enc
	return json.Marshal(m)
}
