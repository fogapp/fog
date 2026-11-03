package objstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4 for S3: the request is canonicalized, hashed,
// and HMAC-chained under a key derived from the secret, date, region and
// service. S3's flavor differs from the generic one in two ways worth
// naming: the URI path is encoded once, not twice, and the payload hash
// header is mandatory.

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds X-Amz-Date, X-Amz-Content-Sha256 and Authorization to req.
func (c *Client) sign(req *http.Request, payloadHash string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	// Signed headers: host, every x-amz-*, and the content headers that
	// affect what the service stores or returns.
	signed := map[string]string{"host": host}
	for name, vals := range req.Header {
		lname := strings.ToLower(name)
		switch {
		case strings.HasPrefix(lname, "x-amz-"), lname == "content-type", lname == "content-md5", lname == "range":
			signed[lname] = strings.Join(trimAll(vals), ",")
		}
	}
	names := make([]string, 0, len(signed))
	for n := range signed {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + signed[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonPath := escapePath(req.URL.Path)
	if canonPath == "" {
		canonPath = "/"
	}
	canonQuery := canonicalQuery(req.URL.Query())

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonPath,
		canonQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + c.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, c.cfg.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.cfg.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func trimAll(vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		// Sequential spaces collapse to one, per the spec.
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return out
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalQuery encodes query parameters the SigV4 way: keys sorted,
// values sorted within a key, every byte outside the unreserved set
// percent-encoded (space is %20, never +), and "key=" for empty values.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// escapePath encodes a URI path segment by segment, keeping the slashes.
func escapePath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return strings.Join(segs, "/")
}

// awsEscape percent-encodes everything except RFC 3986 unreserved
// characters (A-Z a-z 0-9 - . _ ~). url.PathEscape is close but leaves
// several reserved characters alone; S3 signatures need this exact set.
func awsEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}
