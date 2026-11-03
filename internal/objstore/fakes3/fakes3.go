// Package fakes3 is an in-process S3 endpoint for tests: one bucket,
// path-style, with the semantics the offsite layer depends on modelled
// faithfully — versioning with delete markers, Object Lock in compliance
// mode, a credential that may or may not hold the version-delete right,
// multipart uploads, and paged listings. It is not a general S3; it is
// the subset objstore speaks, so a test against it is a test of what fog
// will actually do to a bucket.
package fakes3

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server is the fake. Configure the exported fields before serving.
type Server struct {
	Bucket string
	// AccessKey, when set, must appear in every request's credential
	// scope; a mismatch answers 403 InvalidAccessKeyId.
	AccessKey string
	// Versioning makes Delete write a marker instead of removing.
	Versioning bool
	// Lock enables Object Lock, optionally with a bucket default.
	Lock LockConfig
	// CanDeleteVersions models the credential's deleteFiles right.
	CanDeleteVersions bool
	// DenyHide refuses a plain delete (a hide on a versioned bucket) — a
	// provider that wants the delete right for markers too.
	DenyHide bool
	// PageSize caps listing pages (default 1000) — small values exercise
	// the client's pagination.
	PageSize int
	// Now is the clock; defaults to time.Now.
	Now func() time.Time

	failures  atomic.Int32
	failOp    string
	failAfter int
	failSeen  int
	mu        sync.Mutex
	objects   map[string][]*version
	uploads   map[string]*upload
	seq       int
	calls     map[string]int
	keys      map[string]Rights
}

// Rights are what one extra access key may do; see AddKey.
type Rights struct {
	CanDeleteVersions bool
	DenyHide          bool
}

// AddKey accepts another access key alongside AccessKey, with its own
// rights — a key minted while the fake is serving.
func (s *Server) AddKey(id string, r Rights) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string]Rights{}
	}
	s.keys[id] = r
}

// LockConfig mirrors a bucket's Object Lock configuration.
type LockConfig struct {
	Enabled bool
	Mode    string
	Days    int
}

type version struct {
	id          string
	data        []byte
	modified    time.Time
	marker      bool
	contentType string
	meta        map[string]string
	lockMode    string
	retainUntil time.Time
	etag        string
}

type upload struct {
	key         string
	parts       map[int]*version
	contentType string
	meta        map[string]string
	lockMode    string
	retainUntil time.Time
}

// New returns a fake for one bucket with versioning on and no lock — the
// B2 default shape.
func New(bucket string) *Server {
	return &Server{Bucket: bucket, Versioning: true, PageSize: 1000, objects: map[string][]*version{}, uploads: map[string]*upload{}, calls: map[string]int{}}
}

// FailNext makes the next n requests answer 503 SlowDown before anything
// else happens — the retry path's test hook.
func (s *Server) FailNext(n int) { s.failures.Store(int32(n)) }

// Calls reports how many requests of each op were served.
func (s *Server) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// Versions lists a key's versions oldest first, for assertions: each entry
// is the version id, with "marker:" prefixed for delete markers.
func (s *Server) Versions(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, v := range s.objects[key] {
		if v.marker {
			out = append(out, "marker:"+v.id)
		} else {
			out = append(out, v.id)
		}
	}
	return out
}

// Unhide removes the newest delete marker on key — what an operator does
// in the provider's console after an incident.
func (s *Server) Unhide(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.objects[key]
	if n := len(vs); n > 0 && vs[n-1].marker {
		s.objects[key] = vs[:n-1]
	}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) nextID() string {
	s.seq++
	return fmt.Sprintf("v%06d", s.seq)
}

func (s *Server) count(op string) {
	s.mu.Lock()
	s.calls[op]++
	s.mu.Unlock()
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	xml.NewEncoder(w).Encode(s3Error{Code: code, Message: msg})
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(v)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.failures.Load() > 0 {
		s.failures.Add(-1)
		writeError(w, http.StatusServiceUnavailable, "SlowDown", "injected failure")
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") {
		writeError(w, http.StatusForbidden, "AccessDenied", "unsigned request")
		return
	}
	cred, _, _ := strings.Cut(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential="), "/")
	s.mu.Lock()
	rights, extra := s.keys[cred]
	s.mu.Unlock()
	if !extra {
		if s.AccessKey != "" && cred != s.AccessKey {
			writeError(w, http.StatusForbidden, "InvalidAccessKeyId", "unknown access key")
			return
		}
		rights = Rights{CanDeleteVersions: s.CanDeleteVersions, DenyHide: s.DenyHide}
	}
	if r.Header.Get("X-Amz-Content-Sha256") == "" || r.Header.Get("X-Amz-Date") == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing SigV4 headers")
		return
	}
	// Path-style: /bucket[/key].
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != s.Bucket {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "no such bucket")
		return
	}
	q := r.URL.Query()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		sum := sha256.Sum256(body)
		if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) && got != "UNSIGNED-PAYLOAD" {
			writeError(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "payload hash does not match body")
			return
		}
	}

	// A Content-MD5 that is sent must match; B2 requires one on any upload
	// that sets Object Lock (checked in put, uploadPart and putRetention).
	if md := r.Header.Get("Content-MD5"); md != "" {
		sum := md5.Sum(body)
		if md != base64.StdEncoding.EncodeToString(sum[:]) {
			writeError(w, http.StatusBadRequest, "BadDigest", "the Content-MD5 you specified did not match what was received")
			return
		}
	}
	op := opOf(r.Method, key, q)
	s.count(op)
	if s.shouldFail(op) {
		writeError(w, http.StatusServiceUnavailable, "SlowDown", "injected failure for "+op)
		return
	}
	switch op {
	case "create-multipart":
		s.createMultipart(w, r, key)
	case "upload-part":
		if s.lockedUpload(q.Get("uploadId")) && needsMD5(w, r) {
			return
		}
		s.uploadPart(w, r, key, q.Get("uploadId"), q.Get("partNumber"), body)
	case "complete-multipart":
		s.completeMultipart(w, key, q.Get("uploadId"), body)
	case "abort-multipart":
		s.abortMultipart(w, q.Get("uploadId"))
	case "list-parts":
		s.listParts(w, q.Get("uploadId"))
	case "get-versioning":
		status := ""
		if s.Versioning {
			status = "Enabled"
		}
		writeXML(w, struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
			Status  string   `xml:"Status,omitempty"`
		}{Status: status})
	case "get-object-lock":
		if !s.Lock.Enabled {
			writeError(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket")
			return
		}
		type dr struct {
			Mode string `xml:"Mode"`
			Days int    `xml:"Days"`
		}
		type rule struct {
			DefaultRetention dr `xml:"DefaultRetention"`
		}
		out := struct {
			XMLName           xml.Name `xml:"ObjectLockConfiguration"`
			ObjectLockEnabled string   `xml:"ObjectLockEnabled"`
			Rule              *rule    `xml:"Rule,omitempty"`
		}{ObjectLockEnabled: "Enabled"}
		if s.Lock.Days > 0 {
			out.Rule = &rule{DefaultRetention: dr{Mode: s.Lock.Mode, Days: s.Lock.Days}}
		}
		writeXML(w, out)
	case "put-retention":
		if needsMD5(w, r) {
			return
		}
		s.putRetention(w, key, q.Get("versionId"), body)
	case "list-versions":
		s.listVersions(w, q)
	case "list":
		s.list(w, q)
	case "put":
		s.put(w, r, key, body)
	case "get", "head":
		s.get(w, r, key)
	case "delete-version":
		s.deleteVersion(w, key, q.Get("versionId"), rights)
	case "delete":
		s.delete(w, key, rights)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
	}
}

// opOf names the S3 operation a request is, the way the client's op
// labels do — the vocabulary FailOp and Calls speak.
func opOf(method, key string, q map[string][]string) string {
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch {
	case has("uploads") && method == http.MethodPost:
		return "create-multipart"
	case has("uploadId"):
		switch method {
		case http.MethodPut:
			return "upload-part"
		case http.MethodPost:
			return "complete-multipart"
		case http.MethodDelete:
			return "abort-multipart"
		case http.MethodGet:
			return "list-parts"
		}
	case has("versioning") && method == http.MethodGet:
		return "get-versioning"
	case has("object-lock") && method == http.MethodGet:
		return "get-object-lock"
	case has("retention") && method == http.MethodPut:
		return "put-retention"
	case has("versions") && method == http.MethodGet && key == "":
		return "list-versions"
	case key == "" && method == http.MethodGet:
		return "list"
	case method == http.MethodPut:
		return "put"
	case method == http.MethodGet:
		return "get"
	case method == http.MethodHead:
		return "head"
	case method == http.MethodDelete:
		if has("versionId") {
			return "delete-version"
		}
		return "delete"
	}
	return "unknown"
}

// FailOp makes every request of op after the first `after` successes
// answer 503 — persistently, so the client's retries exhaust and the
// caller sees the failure. FailOp("", 0) clears it.
func (s *Server) FailOp(op string, after int) {
	s.mu.Lock()
	s.failOp, s.failAfter, s.failSeen = op, after, 0
	s.mu.Unlock()
}

func (s *Server) shouldFail(op string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOp == "" || op != s.failOp {
		return false
	}
	s.failSeen++
	return s.failSeen > s.failAfter
}

func etagOf(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// lockFor resolves the lock a new version gets: explicit headers, else
// the bucket default. Headers on a bucket without Object Lock are an
// error, as at AWS and B2.
func (s *Server) lockFor(r *http.Request) (mode string, until time.Time, err error) {
	mode = r.Header.Get("X-Amz-Object-Lock-Mode")
	if mode != "" {
		if !s.Lock.Enabled {
			return "", time.Time{}, fmt.Errorf("InvalidRequest: Bucket is missing Object Lock Configuration")
		}
		until, err = time.Parse(time.RFC3339, r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"))
		if err != nil {
			return "", time.Time{}, fmt.Errorf("InvalidArgument: bad retain-until date")
		}
		return mode, until, nil
	}
	if s.Lock.Enabled && s.Lock.Days > 0 {
		return s.Lock.Mode, s.now().Add(time.Duration(s.Lock.Days) * 24 * time.Hour), nil
	}
	return "", time.Time{}, nil
}

func metaOf(h http.Header) map[string]string {
	var m map[string]string
	for k, v := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			if m == nil {
				m = map[string]string{}
			}
			m[strings.ToLower(k[len("x-amz-meta-"):])] = v[0]
		}
	}
	return m
}

func (s *Server) lockedUpload(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	up := s.uploads[id]
	return up != nil && up.lockMode != ""
}

// needsMD5 answers B2's refusal of a locked upload without a digest.
func needsMD5(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Content-MD5") != "" {
		return false
	}
	writeError(w, http.StatusBadRequest, "InvalidRequest", "Content-MD5 OR x-amz-checksum- HTTP header is required for Put Object requests with Object Lock parameters")
	return true
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, key string, body []byte) {
	if r.Header.Get("X-Amz-Object-Lock-Mode") != "" && needsMD5(w, r) {
		return
	}
	mode, until, err := s.lockFor(r)
	if err != nil {
		code, msg, _ := strings.Cut(err.Error(), ": ")
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	s.mu.Lock()
	v := &version{id: s.nextID(), data: body, modified: s.now(), contentType: r.Header.Get("Content-Type"),
		meta: metaOf(r.Header), lockMode: mode, retainUntil: until, etag: etagOf(body)}
	if s.Versioning {
		s.objects[key] = append(s.objects[key], v)
	} else {
		s.objects[key] = []*version{v}
	}
	s.mu.Unlock()
	w.Header().Set("ETag", v.etag)
	if s.Versioning {
		w.Header().Set("X-Amz-Version-Id", v.id)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) latest(key string) *version {
	vs := s.objects[key]
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	v := s.latest(key)
	s.mu.Unlock()
	if v == nil || v.marker {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	h := w.Header()
	h.Set("ETag", v.etag)
	h.Set("Last-Modified", v.modified.UTC().Format(http.TimeFormat))
	if v.contentType != "" {
		h.Set("Content-Type", v.contentType)
	}
	if s.Versioning {
		h.Set("X-Amz-Version-Id", v.id)
	}
	for k, val := range v.meta {
		h.Set("X-Amz-Meta-"+k, val)
	}
	if v.lockMode != "" {
		h.Set("X-Amz-Object-Lock-Mode", v.lockMode)
		h.Set("X-Amz-Object-Lock-Retain-Until-Date", v.retainUntil.UTC().Format(time.RFC3339))
	}
	data := v.data
	status := http.StatusOK
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
		a, b, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
		start, _ := strconv.Atoi(a)
		end := len(data) - 1
		if b != "" {
			end, _ = strconv.Atoi(b)
		}
		if start > end || start >= len(data) {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "")
			return
		}
		if end >= len(data) {
			end = len(data) - 1
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data = data[start : end+1]
		status = http.StatusPartialContent
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		w.Write(data)
	}
}

func (s *Server) delete(w http.ResponseWriter, key string, rights Rights) {
	if rights.DenyHide {
		writeError(w, http.StatusForbidden, "AccessDenied", "the key is missing the deleteFiles capability")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Versioning {
		// A marker on a missing key is still a marker at AWS; B2 answers
		// 404 for hiding a file that does not exist. Model B2: nothing to
		// hide is a 404, which the client treats as already-deleted.
		v := s.latest(key)
		if v == nil || v.marker {
			writeError(w, http.StatusNotFound, "NoSuchKey", "")
			return
		}
		s.objects[key] = append(s.objects[key], &version{id: s.nextID(), marker: true, modified: s.now()})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	delete(s.objects, key)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteVersion(w http.ResponseWriter, key, vid string, rights Rights) {
	if !rights.CanDeleteVersions {
		writeError(w, http.StatusForbidden, "AccessDenied", "the key is missing the deleteFiles capability")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.objects[key]
	for i, v := range vs {
		if v.id != vid {
			continue
		}
		if v.lockMode != "" && v.retainUntil.After(s.now()) {
			writeError(w, http.StatusForbidden, "AccessDenied", "Object is locked until "+v.retainUntil.UTC().Format(time.RFC3339))
			return
		}
		s.objects[key] = append(vs[:i:i], vs[i+1:]...)
		if len(s.objects[key]) == 0 {
			delete(s.objects, key)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusNotFound, "NoSuchVersion", "")
}

func (s *Server) putRetention(w http.ResponseWriter, key, vid string, body []byte) {
	if !s.Lock.Enabled {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Bucket is missing Object Lock Configuration")
		return
	}
	var req struct {
		Mode            string `xml:"Mode"`
		RetainUntilDate string `xml:"RetainUntilDate"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	until, err := time.Parse(time.RFC3339, req.RetainUntilDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "bad RetainUntilDate")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v *version
	if vid == "" {
		v = s.latest(key)
	} else {
		for _, cand := range s.objects[key] {
			if cand.id == vid {
				v = cand
			}
		}
	}
	if v == nil || v.marker {
		writeError(w, http.StatusNotFound, "NoSuchKey", "")
		return
	}
	if v.lockMode == "COMPLIANCE" && until.Before(v.retainUntil) {
		writeError(w, http.StatusForbidden, "AccessDenied", "compliance retention cannot be shortened")
		return
	}
	v.lockMode, v.retainUntil = req.Mode, until
	w.WriteHeader(http.StatusOK)
}

func (s *Server) sortedKeys() []string {
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) pageSize(q map[string][]string) int {
	n := s.PageSize
	if n <= 0 {
		n = 1000
	}
	if v := q["max-keys"]; len(v) > 0 {
		if m, err := strconv.Atoi(v[0]); err == nil && m > 0 && m < n {
			n = m
		}
	}
	return n
}

func (s *Server) list(w http.ResponseWriter, q map[string][]string) {
	prefix, delim, after := first(q, "prefix"), first(q, "delimiter"), first(q, "continuation-token")
	limit := s.pageSize(q)
	type content struct {
		Key          string `xml:"Key"`
		Size         int    `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	}
	type cp struct {
		Prefix string `xml:"Prefix"`
	}
	out := struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
		CommonPrefixes        []cp      `xml:"CommonPrefixes"`
	}{}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	n := 0
	last := ""
	for _, k := range s.sortedKeys() {
		if !strings.HasPrefix(k, prefix) || k <= after {
			continue
		}
		v := s.latest(k)
		if v == nil || v.marker {
			continue
		}
		entry := k
		if delim != "" {
			rest := k[len(prefix):]
			if i := strings.Index(rest, delim); i >= 0 {
				entry = prefix + rest[:i+len(delim)]
				if seen[entry] {
					continue
				}
			}
		}
		if n == limit {
			out.IsTruncated, out.NextContinuationToken = true, last
			break
		}
		n++
		last = k
		if entry != k {
			seen[entry] = true
			out.CommonPrefixes = append(out.CommonPrefixes, cp{Prefix: entry})
			continue
		}
		out.Contents = append(out.Contents, content{Key: k, Size: len(v.data), ETag: v.etag, LastModified: v.modified.UTC().Format(time.RFC3339)})
	}
	writeXML(w, out)
}

func (s *Server) listVersions(w http.ResponseWriter, q map[string][]string) {
	prefix, keyMarker, verMarker := first(q, "prefix"), first(q, "key-marker"), first(q, "version-id-marker")
	limit := s.pageSize(q)
	type ver struct {
		Key          string `xml:"Key"`
		VersionID    string `xml:"VersionId"`
		IsLatest     bool   `xml:"IsLatest"`
		Size         int    `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	}
	type marker struct {
		Key          string `xml:"Key"`
		VersionID    string `xml:"VersionId"`
		IsLatest     bool   `xml:"IsLatest"`
		LastModified string `xml:"LastModified"`
	}
	out := struct {
		XMLName             xml.Name `xml:"ListVersionsResult"`
		IsTruncated         bool     `xml:"IsTruncated"`
		NextKeyMarker       string   `xml:"NextKeyMarker,omitempty"`
		NextVersionIDMarker string   `xml:"NextVersionIdMarker,omitempty"`
		Versions            []ver    `xml:"Version"`
		DeleteMarkers       []marker `xml:"DeleteMarker"`
	}{}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	started := keyMarker == ""
	for _, k := range s.sortedKeys() {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		vs := s.objects[k]
		for i := len(vs) - 1; i >= 0; i-- {
			v := vs[i]
			if !started {
				if k == keyMarker && v.id == verMarker {
					started = true
				}
				continue
			}
			if n == limit {
				out.IsTruncated = true
				writeXML(w, out)
				return
			}
			n++
			out.NextKeyMarker, out.NextVersionIDMarker = k, v.id
			latest := i == len(vs)-1
			if v.marker {
				out.DeleteMarkers = append(out.DeleteMarkers, marker{Key: k, VersionID: v.id, IsLatest: latest, LastModified: v.modified.UTC().Format(time.RFC3339)})
			} else {
				out.Versions = append(out.Versions, ver{Key: k, VersionID: v.id, IsLatest: latest, Size: len(v.data), ETag: v.etag, LastModified: v.modified.UTC().Format(time.RFC3339)})
			}
		}
	}
	out.NextKeyMarker, out.NextVersionIDMarker = "", ""
	writeXML(w, out)
}

func first(q map[string][]string, k string) string {
	if v := q[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// ---- multipart ----

func (s *Server) createMultipart(w http.ResponseWriter, r *http.Request, key string) {
	mode, until, err := s.lockFor(r)
	if err != nil {
		code, msg, _ := strings.Cut(err.Error(), ": ")
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	s.mu.Lock()
	id := "upload-" + s.nextID()
	s.uploads[id] = &upload{key: key, parts: map[int]*version{}, contentType: r.Header.Get("Content-Type"), meta: metaOf(r.Header), lockMode: mode, retainUntil: until}
	s.mu.Unlock()
	writeXML(w, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}{Bucket: s.Bucket, Key: key, UploadID: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, _ *http.Request, key, id, partNum string, body []byte) {
	n, err := strconv.Atoi(partNum)
	if err != nil || n < 1 || n > 10000 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "bad part number")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	up := s.uploads[id]
	if up == nil || up.key != key {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "")
		return
	}
	up.parts[n] = &version{data: body, etag: etagOf(body)}
	w.Header().Set("ETag", up.parts[n].etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) listParts(w http.ResponseWriter, id string) {
	type part struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
		Size       int    `xml:"Size"`
	}
	out := struct {
		XMLName     xml.Name `xml:"ListPartsResult"`
		IsTruncated bool     `xml:"IsTruncated"`
		Parts       []part   `xml:"Part"`
	}{}
	s.mu.Lock()
	defer s.mu.Unlock()
	up := s.uploads[id]
	if up == nil {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "")
		return
	}
	nums := make([]int, 0, len(up.parts))
	for n := range up.parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		out.Parts = append(out.Parts, part{PartNumber: n, ETag: up.parts[n].etag, Size: len(up.parts[n].data)})
	}
	writeXML(w, out)
}

func (s *Server) completeMultipart(w http.ResponseWriter, key, id string, body []byte) {
	var req struct {
		Parts []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	up := s.uploads[id]
	if up == nil || up.key != key {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "")
		return
	}
	var data []byte
	var sums []byte
	prev := 0
	for _, p := range req.Parts {
		if p.PartNumber <= prev {
			writeError(w, http.StatusBadRequest, "InvalidPartOrder", "")
			return
		}
		prev = p.PartNumber
		part := up.parts[p.PartNumber]
		if part == nil || strings.Trim(part.etag, `"`) != strings.Trim(p.ETag, `"`) {
			writeError(w, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d missing or etag mismatch", p.PartNumber))
			return
		}
		data = append(data, part.data...)
		sum := md5.Sum(part.data)
		sums = append(sums, sum[:]...)
	}
	all := md5.Sum(sums)
	v := &version{id: s.nextID(), data: data, modified: s.now(), contentType: up.contentType, meta: up.meta,
		lockMode: up.lockMode, retainUntil: up.retainUntil, etag: fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(all[:]), len(req.Parts))}
	if s.Versioning {
		s.objects[key] = append(s.objects[key], v)
	} else {
		s.objects[key] = []*version{v}
	}
	delete(s.uploads, id)
	writeXML(w, struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string   `xml:"Bucket"`
		Key     string   `xml:"Key"`
		ETag    string   `xml:"ETag"`
	}{Bucket: s.Bucket, Key: key, ETag: v.etag})
}

func (s *Server) abortMultipart(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.uploads[id]; !ok {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "")
		return
	}
	delete(s.uploads, id)
	w.WriteHeader(http.StatusNoContent)
}
