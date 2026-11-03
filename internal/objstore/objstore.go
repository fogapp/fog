// Package objstore is fog's S3 client: the eleven operations offsite
// backups need, over net/http with SigV4 signing, and nothing else. S3 is
// the one dialect every object store speaks — Backblaze B2, Cloudflare R2,
// Wasabi, Storj, Hetzner, MinIO, Garage, AWS itself — so speaking it
// directly is what makes a target provider-neutral. The vendored SDKs are
// where the provider incompatibilities live (aws-sdk-go-v2's default CRC32
// trailers broke B2 for half of 2025); a client this small has no such
// surprises and no dependency tree.
//
// Semantics the offsite layer relies on, in S3 terms:
//   - Delete by name on a versioned bucket writes a delete marker (B2: a
//     "hide"); the data stays until a version delete or a lifecycle rule
//     removes it. DeleteVersion is the destructive call, and a key
//     without that right cannot destroy anything.
//   - Object Lock is per version: PutOptions.LockMode/RetainUntil at
//     upload, PutRetention to extend.
package objstore

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/backoff"
)

// Config is everything needed to reach one bucket.
type Config struct {
	// Endpoint is the service URL, e.g. https://s3.us-west-004.backblazeb2.com.
	Endpoint string
	// Region is the SigV4 signing region. B2 uses the endpoint's region
	// string; R2 and most S3-compatibles accept "auto" or "us-east-1".
	Region string
	Bucket string
	// AccessKey/SecretKey are the credential pair (B2: applicationKeyId and
	// applicationKey).
	AccessKey string
	SecretKey string
	// PathStyle addresses the bucket in the path (endpoint/bucket/key)
	// rather than the host (bucket.endpoint/key). Path style works
	// everywhere; virtual-hosted style is what AWS pushes.
	PathStyle bool
	// HTTPClient overrides the transport; nil uses a client with sane
	// timeouts for large transfers.
	HTTPClient *http.Client
	// DialControl, when set (and HTTPClient is not), vets the address of every
	// TCP connection the client makes — including each redirect hop, dialed
	// anew — after DNS resolution. The pre-setup recovery door uses it to pin
	// connections to public addresses, so a 307 or DNS rebinding cannot reach
	// an internal host that a URL-level check (on the first host only) misses.
	DialControl func(network, address string) error
}

// Client talks to one bucket.
type Client struct {
	cfg  Config
	base *url.URL
	http *http.Client
	// now is the signing clock — a var so tests can pin it to a known
	// vector's date.
	now func() time.Time
	// retries bounds attempts on retryable failures (5xx, throttling,
	// transport errors). Attempt 1 plus retries-1 retries.
	retries int
}

// New validates the config and returns a client. No network call happens
// here; Probe or the first operation is where the credentials are tried.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("endpoint, bucket, access key and secret key are all required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("endpoint must be a URL like https://s3.us-west-004.backblazeb2.com")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		tr := &http.Transport{
			// No overall timeout: a multi-GB part upload legitimately
			// takes minutes. The context bounds each operation instead.
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 2 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
			TLSHandshakeTimeout:   30 * time.Second,
		}
		hc = &http.Client{Transport: tr}
		if cfg.DialControl != nil {
			// A proxy would dial on our behalf where the Control cannot see
			// the real address, so it is turned off for a guarded client.
			tr.Proxy = nil
			tr.DialContext = (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   func(network, address string, _ syscall.RawConn) error { return cfg.DialControl(network, address) },
			}).DialContext
			hc.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil // each hop dials anew, so DialControl re-checks it
			}
		}
	}
	return &Client{cfg: cfg, base: u, http: hc, now: time.Now, retries: 4}, nil
}

// Bucket is the configured bucket name.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// ---- errors ----

// Error is a non-2xx answer from the service, with S3's error envelope
// when it sent one.
type Error struct {
	Status  int
	Code    string
	Message string
	Op      string
	Key     string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Op)
	if e.Key != "" {
		b.WriteString(" " + e.Key)
	}
	fmt.Fprintf(&b, ": HTTP %d", e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// ErrNotFound is returned (wrapped in *Error via errors.Is) for a missing
// object or bucket.
var ErrNotFound = errors.New("not found")

// ErrAccessDenied is the credential lacking a right — the answer the probe
// wants to see for a version delete.
var ErrAccessDenied = errors.New("access denied")

func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound || e.Code == "NoSuchKey" || e.Code == "NoSuchBucket" || e.Code == "NotFound"
	case ErrAccessDenied:
		return e.Status == http.StatusForbidden || e.Code == "AccessDenied" || e.Code == "UnauthorizedAccess"
	}
	return false
}

// retryable: throttling and server-side failures are worth another try;
// anything 4xx is a fact about the request.
func (e *Error) retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Code == "SlowDown" || e.Code == "RequestTimeout"
}

// ---- object metadata ----

// Object is a listing entry.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Version is a ListVersions entry: an object version or a delete marker.
type Version struct {
	Key          string
	VersionID    string
	IsLatest     bool
	DeleteMarker bool
	Size         int64
	ETag         string
	LastModified time.Time
}

// Info is what HEAD/GET report about one object.
type Info struct {
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
	VersionID    string
	Metadata     map[string]string
	// Object Lock state, zero when unlocked.
	LockMode    string
	RetainUntil time.Time
}

// PutOptions shapes an upload.
type PutOptions struct {
	ContentType string
	// Metadata becomes x-amz-meta-* headers. Values must be ASCII.
	Metadata map[string]string
	// LockMode is "COMPLIANCE" or "GOVERNANCE"; with RetainUntil it locks
	// the new version on write (the bucket must have Object Lock enabled).
	LockMode    string
	RetainUntil time.Time
}

// Digests are a body's checksums as a request carries them: SHA256 is hex
// (the SigV4 payload hash), MD5 base64 (Content-MD5).
type Digests struct {
	SHA256 string
	MD5    string
}

// DigestedBody is an upload body that already knows its digests — a
// caller that hashed the bytes while writing them out. Put and UploadPart
// send such a body without first reading it through to hash it.
type DigestedBody interface {
	io.ReadSeeker
	Digests() Digests
}

// Part is one completed multipart piece.
type Part struct {
	Number int
	ETag   string
	Size   int64
}

// ---- request plumbing ----

// urlFor builds the object URL. Keys are escaped per segment the way S3
// canonicalizes them (RFC 3986 unreserved set), and the escaped form is
// what goes on the wire, so the signature and the request agree.
func (c *Client) urlFor(key string, query url.Values) *url.URL {
	u := *c.base
	path := u.Path
	if c.cfg.PathStyle {
		path += "/" + c.cfg.Bucket
	} else {
		u.Host = c.cfg.Bucket + "." + u.Host
	}
	if key != "" {
		path += "/" + key
	}
	if path == "" {
		path = "/"
	}
	u.Path = path
	u.RawPath = escapePath(path)
	if len(query) > 0 {
		u.RawQuery = canonicalQuery(query)
	}
	return &u
}

type request struct {
	method  string
	key     string
	query   url.Values
	headers http.Header
	// body is re-sent on retry; nil for no body.
	body io.ReadSeeker
	size int64
	// payloadHash is hex sha256 of body; computed when empty and a body
	// exists.
	payloadHash string
	op          string
}

// do sends one request with retries and returns the response. A non-2xx
// answer becomes *Error (the body is consumed). The caller closes the
// body of a successful response.
func (c *Client) do(ctx context.Context, rq *request) (*http.Response, error) {
	if rq.body == nil {
		if rq.payloadHash == "" {
			rq.payloadHash = emptyPayloadHash
		}
	} else if rq.payloadHash == "" || rq.headers.Get("Content-MD5") == "" {
		// Every body carries Content-MD5: B2 (and AWS) refuse an upload
		// that sets Object Lock without it, and it is a free integrity
		// check on the rest. A body that brings its digests is trusted;
		// otherwise one pass computes both.
		var d Digests
		if db, ok := rq.body.(DigestedBody); ok {
			d = db.Digests()
		}
		if d.SHA256 == "" || d.MD5 == "" {
			var err error
			if d.SHA256, d.MD5, err = hashReadSeeker(rq.body); err != nil {
				return nil, err
			}
		}
		if rq.payloadHash == "" {
			rq.payloadHash = d.SHA256
		}
		if rq.headers == nil {
			rq.headers = http.Header{}
		}
		if rq.headers.Get("Content-MD5") == "" {
			rq.headers.Set("Content-MD5", d.MD5)
		}
	}
	var lastErr error
	for attempt := 0; attempt < c.retries; attempt++ {
		if attempt > 0 {
			// Jittered backoff: 0.5s, 1s, 2s (+ up to 25%).
			d := backoff.Jitter(backoff.Exp(500*time.Millisecond, 30*time.Second, attempt-1))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d):
			}
			if rq.body != nil {
				if _, err := rq.body.Seek(0, io.SeekStart); err != nil {
					return nil, err
				}
			}
		}
		resp, err := c.once(ctx, rq)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var se *Error
		if errors.As(err, &se) && !se.retryable() {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, rq *request) (*http.Response, error) {
	u := c.urlFor(rq.key, rq.query)
	var body io.Reader
	if rq.body != nil {
		body = rq.body
	}
	req, err := http.NewRequestWithContext(ctx, rq.method, u.String(), body)
	if err != nil {
		return nil, err
	}
	// NewRequest re-parses the URL and may drop RawPath in favor of Go's
	// own escaping, which leaves characters like ':' and '@' bare. The
	// signature canonicalizes with the AWS set (sigv4.go), so the wire
	// path is pinned to that same form here.
	req.URL.RawPath = escapePath(req.URL.Path)
	if rq.body != nil {
		req.ContentLength = rq.size
		// A ReadSeeker body must not be wrapped by NewRequest's
		// GetBody-less handling into chunked encoding: S3 needs the length.
		if rq.size == 0 {
			req.Body = http.NoBody
		}
	}
	for k, vs := range rq.headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	c.sign(req, rq.payloadHash, c.now().UTC())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rq.op, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	se := &Error{Status: resp.StatusCode, Op: rq.op, Key: rq.key}
	var env struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(raw, &env) == nil {
		se.Code, se.Message = env.Code, env.Message
	}
	if se.Code == "" && se.Message == "" && len(raw) > 0 && len(raw) < 200 {
		se.Message = strings.TrimSpace(string(raw))
	}
	return nil, se
}

// hashReadSeeker returns the body's hex sha256 (the signature's payload
// hash) and base64 MD5 (Content-MD5), and rewinds it.
func hashReadSeeker(r io.ReadSeeker) (sha string, md string, err error) {
	h, m := sha256.New(), md5.New()
	if _, err := io.Copy(io.MultiWriter(h, m), r); err != nil {
		return "", "", err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h.Sum(nil)), base64.StdEncoding.EncodeToString(m.Sum(nil)), nil
}

func decodeXML(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return xml.NewDecoder(resp.Body).Decode(v)
}

// ---- objects ----

// Put uploads one object in a single request. body is read twice — once
// to hash it for the signature, once to send — unless it is a
// DigestedBody; either way it must seek, since a retry re-sends it. Spool
// to a file for anything that does not fit in memory. Objects over
// ~100 MB belong in a multipart upload.
func (c *Client) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, opts PutOptions) (etag string, err error) {
	h := http.Header{}
	applyPutOptions(h, opts)
	resp, err := c.do(ctx, &request{method: http.MethodPut, key: key, headers: h, body: body, size: size, op: "put"})
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// PutBytes is Put for small in-memory objects (manifests, indexes).
func (c *Client) PutBytes(ctx context.Context, key string, data []byte, opts PutOptions) (string, error) {
	return c.Put(ctx, key, bytes.NewReader(data), int64(len(data)), opts)
}

func applyPutOptions(h http.Header, opts PutOptions) {
	if opts.ContentType != "" {
		h.Set("Content-Type", opts.ContentType)
	}
	for k, v := range opts.Metadata {
		h.Set("X-Amz-Meta-"+k, v)
	}
	if opts.LockMode != "" && !opts.RetainUntil.IsZero() {
		h.Set("X-Amz-Object-Lock-Mode", opts.LockMode)
		h.Set("X-Amz-Object-Lock-Retain-Until-Date", opts.RetainUntil.UTC().Format(time.RFC3339))
	}
}

// Get streams an object. rng is an HTTP Range value ("bytes=0-1023") or "".
func (c *Client) Get(ctx context.Context, key string, rng string) (io.ReadCloser, Info, error) {
	h := http.Header{}
	if rng != "" {
		h.Set("Range", rng)
	}
	resp, err := c.do(ctx, &request{method: http.MethodGet, key: key, headers: h, op: "get"})
	if err != nil {
		return nil, Info{}, err
	}
	return resp.Body, infoFrom(resp.Header), nil
}

// GetBytes reads a whole (small) object.
func (c *Client) GetBytes(ctx context.Context, key string, limit int64) ([]byte, Info, error) {
	rc, info, err := c.Get(ctx, key, "")
	if err != nil {
		return nil, info, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, info, err
	}
	if int64(len(data)) > limit {
		return nil, info, fmt.Errorf("get %s: object exceeds %d bytes", key, limit)
	}
	return data, info, nil
}

// Head reports an object's metadata; ErrNotFound when absent (a hidden
// object — newest version a delete marker — is absent).
func (c *Client) Head(ctx context.Context, key string) (Info, error) {
	resp, err := c.do(ctx, &request{method: http.MethodHead, key: key, op: "head"})
	if err != nil {
		return Info{}, err
	}
	resp.Body.Close()
	return infoFrom(resp.Header), nil
}

func infoFrom(h http.Header) Info {
	info := Info{
		ETag:        strings.Trim(h.Get("ETag"), `"`),
		ContentType: h.Get("Content-Type"),
		VersionID:   h.Get("X-Amz-Version-Id"),
		LockMode:    h.Get("X-Amz-Object-Lock-Mode"),
	}
	if v := h.Get("Content-Length"); v != "" {
		info.Size, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := h.Get("Last-Modified"); v != "" {
		info.LastModified, _ = http.ParseTime(v)
	}
	if v := h.Get("X-Amz-Object-Lock-Retain-Until-Date"); v != "" {
		info.RetainUntil = parseAmzTime(v)
	}
	for k, vs := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") && len(vs) > 0 {
			if info.Metadata == nil {
				info.Metadata = map[string]string{}
			}
			info.Metadata[strings.ToLower(k[len("x-amz-meta-"):])] = vs[0]
		}
	}
	return info
}

func parseAmzTime(v string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Delete removes an object by name. On a versioned bucket this writes a
// delete marker (B2: hides the file) and destroys nothing.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, &request{method: http.MethodDelete, key: key, op: "delete"})
	if err != nil {
		// Deleting what is already gone is success: the state is the one
		// asked for.
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// DeleteVersion permanently removes one version. This is the destructive
// call the offsite layer never makes in normal operation; the probe uses it
// to prove the credential *cannot*.
func (c *Client) DeleteVersion(ctx context.Context, key, versionID string) error {
	q := url.Values{"versionId": {versionID}}
	resp, err := c.do(ctx, &request{method: http.MethodDelete, key: key, query: q, op: "delete-version"})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ---- listing ----

// List walks every object under prefix, in key order, calling fn for each.
// Pagination is internal; fn returning an error stops the walk.
func (c *Client) List(ctx context.Context, prefix string, fn func(Object) error) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, &request{method: http.MethodGet, query: q, op: "list"})
		if err != nil {
			return err
		}
		var page listBucketResult
		if err := decodeXML(resp, &page); err != nil {
			return fmt.Errorf("list: decoding response: %w", err)
		}
		for _, o := range page.Contents {
			if err := fn(Object{Key: o.Key, Size: o.Size, ETag: strings.Trim(o.ETag, `"`), LastModified: parseAmzTime(o.LastModified)}); err != nil {
				return err
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}

// ListPrefixes returns the "subdirectories" directly under prefix — the
// common prefixes of a delimited listing — with prefix and delimiter
// stripped. Cheap on every provider; the shape a restore wizard wants
// ("which apps are in this locker").
func (c *Client) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "delimiter": {"/"}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, &request{method: http.MethodGet, query: q, op: "list"})
		if err != nil {
			return nil, err
		}
		var page listBucketResult
		if err := decodeXML(resp, &page); err != nil {
			return nil, fmt.Errorf("list: decoding response: %w", err)
		}
		for _, p := range page.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(p.Prefix, prefix), "/")
			if name != "" {
				out = append(out, name)
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Strings(out)
	return out, nil
}

// ListVersions walks every version and delete marker under prefix.
func (c *Client) ListVersions(ctx context.Context, prefix string, fn func(Version) error) error {
	keyMarker, verMarker := "", ""
	for {
		q := url.Values{"versions": {""}, "prefix": {prefix}, "max-keys": {"1000"}}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
		}
		if verMarker != "" {
			q.Set("version-id-marker", verMarker)
		}
		resp, err := c.do(ctx, &request{method: http.MethodGet, query: q, op: "list-versions"})
		if err != nil {
			return err
		}
		var page listVersionsResult
		if err := decodeXML(resp, &page); err != nil {
			return fmt.Errorf("list-versions: decoding response: %w", err)
		}
		// Versions and markers arrive as two lists; merge by key then
		// recency so callers see one stream in a sensible order.
		var all []Version
		for _, v := range page.Versions {
			all = append(all, Version{Key: v.Key, VersionID: v.VersionID, IsLatest: v.IsLatest, Size: v.Size,
				ETag: strings.Trim(v.ETag, `"`), LastModified: parseAmzTime(v.LastModified)})
		}
		for _, d := range page.DeleteMarkers {
			all = append(all, Version{Key: d.Key, VersionID: d.VersionID, IsLatest: d.IsLatest, DeleteMarker: true,
				LastModified: parseAmzTime(d.LastModified)})
		}
		sort.SliceStable(all, func(i, j int) bool {
			if all[i].Key != all[j].Key {
				return all[i].Key < all[j].Key
			}
			if all[i].IsLatest != all[j].IsLatest {
				return all[i].IsLatest
			}
			return all[i].LastModified.After(all[j].LastModified)
		})
		for _, v := range all {
			if err := fn(v); err != nil {
				return err
			}
		}
		if !page.IsTruncated {
			return nil
		}
		keyMarker, verMarker = page.NextKeyMarker, page.NextVersionIDMarker
		if keyMarker == "" {
			return nil
		}
	}
}

// ---- multipart ----

// CreateMultipart starts a multipart upload and returns its id. Lock
// options apply to the completed object.
func (c *Client) CreateMultipart(ctx context.Context, key string, opts PutOptions) (string, error) {
	h := http.Header{}
	applyPutOptions(h, opts)
	q := url.Values{"uploads": {""}}
	resp, err := c.do(ctx, &request{method: http.MethodPost, key: key, query: q, headers: h, op: "create-multipart"})
	if err != nil {
		return "", err
	}
	var out initiateMultipartResult
	if err := decodeXML(resp, &out); err != nil {
		return "", fmt.Errorf("create-multipart: decoding response: %w", err)
	}
	if out.UploadID == "" {
		return "", errors.New("create-multipart: no upload id in response")
	}
	return out.UploadID, nil
}

// UploadPart sends one part (numbered from 1). Parts must be at least 5 MB
// except the last. Returns the part's ETag for CompleteMultipart. body is
// hashed first unless it is a DigestedBody.
func (c *Client) UploadPart(ctx context.Context, key, uploadID string, number int, body io.ReadSeeker, size int64) (Part, error) {
	q := url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {uploadID}}
	resp, err := c.do(ctx, &request{method: http.MethodPut, key: key, query: q, body: body, size: size, op: "upload-part"})
	if err != nil {
		return Part{}, err
	}
	resp.Body.Close()
	return Part{Number: number, ETag: strings.Trim(resp.Header.Get("ETag"), `"`), Size: size}, nil
}

// ListParts reports the parts already received for an upload — how a
// resumed upload learns where it left off without local state.
func (c *Client) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	var out []Part
	marker := 0
	for {
		q := url.Values{"uploadId": {uploadID}, "max-parts": {"1000"}}
		if marker > 0 {
			q.Set("part-number-marker", strconv.Itoa(marker))
		}
		resp, err := c.do(ctx, &request{method: http.MethodGet, key: key, query: q, op: "list-parts"})
		if err != nil {
			return nil, err
		}
		var page listPartsResult
		if err := decodeXML(resp, &page); err != nil {
			return nil, fmt.Errorf("list-parts: decoding response: %w", err)
		}
		for _, p := range page.Parts {
			out = append(out, Part{Number: p.PartNumber, ETag: strings.Trim(p.ETag, `"`), Size: p.Size})
		}
		if !page.IsTruncated || page.NextPartNumberMarker == 0 {
			break
		}
		marker = page.NextPartNumberMarker
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// CompleteMultipart assembles the parts into the object.
func (c *Client) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (etag string, err error) {
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	var body completeMultipartUpload
	for _, p := range parts {
		body.Parts = append(body.Parts, completePart{PartNumber: p.Number, ETag: p.ETag})
	}
	raw, err := xml.Marshal(body)
	if err != nil {
		return "", err
	}
	q := url.Values{"uploadId": {uploadID}}
	h := http.Header{"Content-Type": {"application/xml"}}
	resp, err := c.do(ctx, &request{method: http.MethodPost, key: key, query: q, headers: h, body: bytes.NewReader(raw), size: int64(len(raw)), op: "complete-multipart"})
	if err != nil {
		return "", err
	}
	// S3 can answer 200 with an error body for a failed completion (the
	// infamous "200 OK error"); the result element is the proof of success.
	var out completeMultipartResult
	raw2, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if err := xml.Unmarshal(raw2, &out); err != nil || out.XMLName.Local != "CompleteMultipartUploadResult" {
		var env struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		xml.Unmarshal(raw2, &env)
		return "", &Error{Status: resp.StatusCode, Code: env.Code, Message: env.Message, Op: "complete-multipart", Key: key}
	}
	return strings.Trim(out.ETag, `"`), nil
}

// AbortMultipart discards an unfinished upload and its parts.
func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	q := url.Values{"uploadId": {uploadID}}
	resp, err := c.do(ctx, &request{method: http.MethodDelete, key: key, query: q, op: "abort-multipart"})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// ---- bucket facts ----

// Versioning reports whether the bucket keeps versions ("Enabled",
// "Suspended", or "" for never enabled).
func (c *Client) Versioning(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, &request{method: http.MethodGet, query: url.Values{"versioning": {""}}, op: "get-versioning"})
	if err != nil {
		return "", err
	}
	var out versioningConfiguration
	if err := decodeXML(resp, &out); err != nil {
		return "", fmt.Errorf("get-versioning: decoding response: %w", err)
	}
	return out.Status, nil
}

// LockConfig is a bucket's Object Lock configuration.
type LockConfig struct {
	Enabled bool `json:"enabled"`
	// Default retention applied to new uploads, when the bucket has one.
	Mode  string `json:"mode,omitempty"`
	Days  int    `json:"days,omitempty"`
	Years int    `json:"years,omitempty"`
}

// ObjectLock reads the bucket's Object Lock configuration. A bucket that
// never had it enabled reports Enabled=false (S3 answers 404 for that;
// it is a fact, not a failure).
func (c *Client) ObjectLock(ctx context.Context) (LockConfig, error) {
	resp, err := c.do(ctx, &request{method: http.MethodGet, query: url.Values{"object-lock": {""}}, op: "get-object-lock"})
	if err != nil {
		var se *Error
		if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Code == "ObjectLockConfigurationNotFoundError") {
			return LockConfig{}, nil
		}
		return LockConfig{}, err
	}
	var out objectLockConfiguration
	if err := decodeXML(resp, &out); err != nil {
		return LockConfig{}, fmt.Errorf("get-object-lock: decoding response: %w", err)
	}
	cfg := LockConfig{Enabled: strings.EqualFold(out.ObjectLockEnabled, "Enabled")}
	if out.Rule != nil {
		cfg.Mode, cfg.Days, cfg.Years = out.Rule.DefaultRetention.Mode, out.Rule.DefaultRetention.Days, out.Rule.DefaultRetention.Years
	}
	return cfg, nil
}

// PutRetention sets (or, in compliance mode, extends) an object version's
// lock. versionID "" addresses the current version.
func (c *Client) PutRetention(ctx context.Context, key, versionID, mode string, until time.Time) error {
	q := url.Values{"retention": {""}}
	if versionID != "" {
		q.Set("versionId", versionID)
	}
	raw, err := xml.Marshal(retention{Mode: mode, RetainUntilDate: until.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	// Content-MD5, mandatory on this call at AWS, is added by do.
	h := http.Header{"Content-Type": {"application/xml"}}
	resp, err := c.do(ctx, &request{method: http.MethodPut, key: key, query: q, headers: h, body: bytes.NewReader(raw), size: int64(len(raw)), op: "put-retention"})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
