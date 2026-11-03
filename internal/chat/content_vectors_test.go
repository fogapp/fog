package chat

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// The vectors the SDK's tests read too: one file, so the Go bot and every
// TypeScript client agree on content byte for byte.
const contentVectorsPath = "../../clients/chat-sdk/test/vectors/content.json"

type contentVectors struct {
	About string          `json:"about"`
	Cases []contentVector `json:"cases"`
}

type contentVector struct {
	Name    string          `json:"name"`
	Content json.RawMessage `json:"content"`
	CBOR    string          `json:"cbor"`
}

func TestContentVectors(t *testing.T) {
	raw, err := os.ReadFile(contentVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var vs contentVectors
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	update := os.Getenv("UPDATE_VECTORS") == "1"
	for i, v := range vs.Cases {
		t.Run(v.Name, func(t *testing.T) {
			generic, err := vectorValue(v.Content)
			if err != nil {
				t.Fatal(err)
			}
			// The JSON is the map on the wire: its canonical encoding is
			// what the hex must be.
			want, err := contentEnc.Marshal(generic)
			if err != nil {
				t.Fatal(err)
			}
			if update {
				vs.Cases[i].CBOR = hex.EncodeToString(want)
				return
			}
			got, err := hex.DecodeString(v.CBOR)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("hex is not the canonical encoding of the content: run with UPDATE_VECTORS=1")
			}
			// Content must carry every field through a round trip: a field
			// the struct lacks would be dropped here.
			c, err := DecodeContent(got)
			if err != nil {
				t.Fatal(err)
			}
			again, err := EncodeContent(c)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, got) {
				t.Fatalf("round trip changed the bytes:\n got %x\nwant %x", again, got)
			}
		})
	}
	if update {
		out, err := json.MarshalIndent(vs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(contentVectorsPath, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// vectorValue turns the JSON form into what CBOR encodes: integers as
// integers and {"$hex": …} objects as byte strings.
func vectorValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return convertVector(v)
}

func convertVector(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return nil, fmt.Errorf("content has no floats: %s", x)
		}
		return x.Int64()
	case map[string]any:
		if h, ok := x["$hex"].(string); ok && len(x) == 1 {
			return hex.DecodeString(h)
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			c, err := convertVector(e)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			c, err := convertVector(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return v, nil
}

// TestContentRefusesReservedTypes: "membership" is a line a client derives
// from a commit it verified; no member may send it, or a type nobody knows.
func TestContentRefusesReservedTypes(t *testing.T) {
	for _, typ := range []string{"membership", "nonsense", ""} {
		b, err := cbor.Marshal(map[string]any{"v": 1, "type": typ})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeContent(b); err == nil {
			t.Errorf("content of type %q decoded", typ)
		}
	}
}
