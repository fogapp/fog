package objstore

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The published S3 SigV4 examples ("Examples: Signature Calculations in
// AWS Signature Version 4", Amazon S3 API Reference). A signer that
// reproduces these signatures byte for byte is one every S3-speaking
// provider accepts; a signer tested only against its own fake proves
// nothing about the wire.
func exampleClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{
		Endpoint:  "https://s3.amazonaws.com",
		Region:    "us-east-1",
		Bucket:    "examplebucket",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }
	return c
}

func signatureOf(auth string) string {
	_, sig, _ := strings.Cut(auth, "Signature=")
	return sig
}

func TestSigV4GetObjectVector(t *testing.T) {
	c := exampleClient(t)
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	c.sign(req, emptyPayloadHash, c.now())
	const want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := signatureOf(req.Header.Get("Authorization")); got != want {
		t.Fatalf("GET object signature\n got %s\nwant %s\nauth %s", got, want, req.Header.Get("Authorization"))
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,") {
		t.Fatalf("signed headers: %s", req.Header.Get("Authorization"))
	}
}

func TestSigV4SubresourceVector(t *testing.T) {
	c := exampleClient(t)
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/?lifecycle", nil)
	c.sign(req, emptyPayloadHash, c.now())
	const want = "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"
	if got := signatureOf(req.Header.Get("Authorization")); got != want {
		t.Fatalf("GET ?lifecycle signature\n got %s\nwant %s", got, want)
	}
}

// Query canonicalization: sorted keys, sorted values, AWS escaping, and an
// "=" even for empty values (S3 subresources like ?versions).
func TestCanonicalQuery(t *testing.T) {
	q := url.Values{"prefix": {"J/x y"}, "max-keys": {"2"}, "versions": {""}, "list-type": {"2"}}
	if got := canonicalQuery(q); got != "list-type=2&max-keys=2&prefix=J%2Fx%20y&versions=" {
		t.Fatalf("canonicalQuery = %q", got)
	}
}

func TestAwsEscape(t *testing.T) {
	cases := map[string]string{
		"photos/2026 summer/IMG_001.jpg": "photos%2F2026%20summer%2FIMG_001.jpg",
		"a+b=c&d":                        "a%2Bb%3Dc%26d",
		"tilde~ok":                       "tilde~ok",
		"ünïcode":                        "%C3%BCn%C3%AFcode",
	}
	for in, want := range cases {
		if got := awsEscape(in); got != want {
			t.Errorf("awsEscape(%q) = %q, want %q", in, got, want)
		}
	}
	if got := escapePath("/b/photos/2026 summer/x:y@z"); got != "/b/photos/2026%20summer/x%3Ay%40z" {
		t.Errorf("escapePath = %q", got)
	}
}
