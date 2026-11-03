package fed

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The vectors clients/fogfed checks too (test/parity_test.rb): what this
// side's RFC 3339 parse and envelope head decoders take and refuse, the
// Ruby gem takes and refuses. A case here that Go decides differently means
// the vectors are wrong, and both sides would drift with them.
func TestFogfed1ParserParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "clients", "fogfed", "conformance", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	type headCase struct {
		Name    string              `json:"name"`
		HeadB64 string              `json:"head_b64"`
		Method  string              `json:"method"`
		Target  string              `json:"target"`
		Headers map[string][]string `json:"headers"`
		Status  int                 `json:"status"`
	}
	type heads struct {
		Requests []headCase `json:"requests"`
		Answers  []headCase `json:"answers"`
	}
	var v struct {
		Timestamps struct {
			Accept []string `json:"accept"`
			Reject []string `json:"reject"`
		} `json:"timestamps"`
		Rejections heads `json:"envelope_rejections"`
		Accepts    heads `json:"envelope_accepts"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Timestamps.Reject) == 0 || len(v.Rejections.Requests) == 0 || len(v.Accepts.Answers) == 0 {
		t.Fatal("no parity vectors — regenerate conformance/vectors.json")
	}

	for _, ts := range v.Timestamps.Accept {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("timestamp %q: Go refuses it (%v), the vectors say accept", ts, err)
		}
	}
	for _, ts := range v.Timestamps.Reject {
		if _, err := time.Parse(time.RFC3339, ts); err == nil {
			t.Errorf("timestamp %q: Go takes it, the vectors say refuse", ts)
		}
	}

	head := func(c headCase) []byte {
		b, err := base64.StdEncoding.DecodeString(c.HeadB64)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		return b
	}
	// The server path: DecodeInnerRequest, then http.NewRequest.
	decodeRequest := func(h []byte) (string, string, http.Header, error) {
		plain := binary.BigEndian.AppendUint32(nil, uint32(len(h)))
		plain = append(append(plain, h...), "body"...)
		method, target, hdr, _, err := DecodeInnerRequest(plain)
		if err == nil {
			_, err = http.NewRequest(method, target, nil)
		}
		return method, target, hdr, err
	}
	answerHead := func(h []byte) (*AnswerHead, error) {
		plain := binary.BigEndian.AppendUint32(nil, uint32(len(h)))
		plain = append(plain, h...)
		plain = binary.BigEndian.AppendUint16(plain, 0)
		return readAnswerHeadAt(bytes.NewReader(plain), nil, "/x", Bound{}, time.Now())
	}
	for _, c := range v.Rejections.Requests {
		if _, _, _, err := decodeRequest(head(c)); err == nil {
			t.Errorf("request head %q: Go takes it, the vectors say refuse", c.Name)
		}
	}
	for _, c := range v.Rejections.Answers {
		if _, err := answerHead(head(c)); err == nil {
			t.Errorf("answer head %q: Go takes it, the vectors say refuse", c.Name)
		}
	}
	for _, c := range v.Accepts.Requests {
		method, target, hdr, err := decodeRequest(head(c))
		if err != nil {
			t.Errorf("request head %q: Go refuses it: %v", c.Name, err)
			continue
		}
		want := http.Header(c.Headers)
		if want == nil {
			want = http.Header{}
		}
		if hdr == nil {
			hdr = http.Header{}
		}
		if method != c.Method || target != c.Target || !reflect.DeepEqual(hdr, want) {
			t.Errorf("request head %q: got %q %q %v, vectors say %q %q %v", c.Name, method, target, hdr, c.Method, c.Target, want)
		}
	}
	for _, c := range v.Accepts.Answers {
		ah, err := answerHead(head(c))
		if err != nil || ah.Status != c.Status {
			t.Errorf("answer head %q: got %+v, %v; vectors say status %d", c.Name, ah, err, c.Status)
		}
	}
}
