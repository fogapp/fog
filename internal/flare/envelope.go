// Package flare is fogd's error tracker: it ingests Sentry-envelope error
// reports from managed apps, groups them by fingerprint, and pushes to ntfy
// on the transitions that matter (it files no notices). The wire format is
// deliberately Sentry's (clients/flare/README.md): the official SDKs are
// drop-in clients, and our own gem speaks the same schema.
package flare

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// EnvelopeItem is one item from a Sentry envelope: a parsed header (only the
// fields we act on) and the raw payload bytes.
type EnvelopeItem struct {
	Type    string
	Payload []byte
}

// Envelope is the parsed form of one POST body.
type Envelope struct {
	// Header is the envelope's own header line; "dsn" is the only key we
	// read (one of the three places an SDK may put its credentials).
	Header map[string]any
	Items  []EnvelopeItem
}

// Bounds on a single envelope. The HTTP handler additionally caps the
// compressed body; these guard the decompressed parse.
const (
	maxItems       = 100
	maxItemPayload = 1 << 20 // Sentry's own per-event item limit
)

// ParseEnvelope reads Sentry's newline-delimited envelope format: a header
// line, then per item a header line and a payload. A payload's extent is its
// header's `length` in bytes when present, otherwise it runs to the next
// newline. Unknown item types are preserved — the caller decides what to
// drop, and deciding takes knowing they were there.
func ParseEnvelope(r io.Reader) (*Envelope, error) {
	br := bufio.NewReaderSize(r, 64<<10)

	headerLine, err := readLine(br)
	if err != nil {
		return nil, fmt.Errorf("envelope header: %w", err)
	}
	env := &Envelope{Header: map[string]any{}}
	if err := json.Unmarshal(headerLine, &env.Header); err != nil {
		return nil, fmt.Errorf("envelope header: %w", err)
	}

	for {
		itemLine, err := readLine(br)
		if err == io.EOF || (err == nil && len(bytes.TrimSpace(itemLine)) == 0) {
			// A trailing newline after the last payload is valid.
			if err == nil {
				continue
			}
			return env, nil
		}
		if err != nil {
			return nil, fmt.Errorf("item header: %w", err)
		}
		var ih struct {
			Type   string `json:"type"`
			Length *int64 `json:"length"`
		}
		if err := json.Unmarshal(itemLine, &ih); err != nil {
			return nil, fmt.Errorf("item header: %w", err)
		}
		if ih.Type == "" {
			return nil, fmt.Errorf("item header missing type")
		}

		var payload []byte
		if ih.Length != nil {
			if *ih.Length < 0 || *ih.Length > maxItemPayload {
				return nil, fmt.Errorf("item %q: length %d out of bounds", ih.Type, *ih.Length)
			}
			payload = make([]byte, *ih.Length)
			if _, err := io.ReadFull(br, payload); err != nil {
				return nil, fmt.Errorf("item %q payload: %w", ih.Type, err)
			}
			// Consume the newline that terminates a length-prefixed payload
			// (absent when the payload ends the envelope).
			if b, err := br.ReadByte(); err == nil && b != '\n' {
				br.UnreadByte()
			}
		} else {
			payload, err = readLine(br)
			if err != nil && err != io.EOF {
				return nil, fmt.Errorf("item %q payload: %w", ih.Type, err)
			}
			if len(payload) > maxItemPayload {
				return nil, fmt.Errorf("item %q: payload exceeds %d bytes", ih.Type, maxItemPayload)
			}
		}
		env.Items = append(env.Items, EnvelopeItem{Type: ih.Type, Payload: payload})
		if len(env.Items) > maxItems {
			return nil, fmt.Errorf("envelope exceeds %d items", maxItems)
		}
		if err == io.EOF {
			return env, nil
		}
	}
}

// readLine reads up to a newline, returning the line without it. io.EOF with
// content means a final unterminated line — valid in this format.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		return nil, err
	}
	return bytes.TrimSuffix(line, []byte("\n")), nil
}

// ExtractKey pulls the client's public key from the three places the Sentry
// transport spec allows: the X-Sentry-Auth header, the sentry_key query
// parameter, or the dsn field of the envelope header. First present wins;
// they're required to agree, and if a client disagrees with itself the
// constant-time compare downstream settles it.
func ExtractKey(h http.Header, query url.Values, envHeader map[string]any) string {
	if auth := h.Get("X-Sentry-Auth"); auth != "" {
		for _, part := range strings.Split(strings.TrimPrefix(auth, "Sentry"), ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k == "sentry_key" {
				return strings.TrimSpace(v)
			}
		}
	}
	if k := query.Get("sentry_key"); k != "" {
		return k
	}
	if dsn, _ := envHeader["dsn"].(string); dsn != "" {
		if u, err := url.Parse(dsn); err == nil && u.User != nil {
			return u.User.Username()
		}
	}
	return ""
}
