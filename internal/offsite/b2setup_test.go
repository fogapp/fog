package offsite

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore/fakes3"
)

// fakeB2 is Backblaze's native API as far as setup speaks it: sign-in,
// buckets, keys. Keys it mints are registered with the S3 fake, with the
// rights their capabilities give.
type fakeB2 struct {
	t       *testing.T
	s3      *fakes3.Server
	s3URL   string
	caps    []string
	scoped  bool
	foreign map[string]bool // bucket names owned by another account
	// hideNeedsDelete models B2 wanting deleteFiles for S3 delete markers.
	hideNeedsDelete bool

	mu      sync.Mutex
	buckets map[string]map[string]any // name → request body (+ bucketId)
	keys    map[string][]string       // id → capabilities
	deleted []string                  // keys and buckets taken back
	seq     int
	srv     *httptest.Server
}

func newFakeB2(t *testing.T, s3 *fakes3.Server, s3URL string) *fakeB2 {
	f := &fakeB2{t: t, s3: s3, s3URL: s3URL, caps: []string{"listBuckets", "writeBuckets", "deleteBuckets", "listKeys", "writeKeys", "deleteKeys"},
		foreign: map[string]bool{}, buckets: map[string]map[string]any{}, keys: map[string][]string{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeB2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := func(status int, code string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code, "message": code})
	}
	op := strings.TrimPrefix(r.URL.Path, "/b2api/v3/")
	if op == "b2_authorize_account" {
		id, secret, _ := r.BasicAuth()
		if id != "master-id" || secret != "master-secret" {
			fail(401, "unauthorized")
			return
		}
		sa := map[string]any{"apiUrl": f.srv.URL, "s3ApiUrl": f.s3URL, "capabilities": f.caps}
		if f.scoped {
			sa["bucketId"] = "b-other"
		}
		json.NewEncoder(w).Encode(map[string]any{"accountId": "acct", "authorizationToken": "tok", "apiInfo": map[string]any{"storageApi": sa}})
		return
	}
	if r.Header.Get("Authorization") != "tok" {
		fail(401, "bad_auth_token")
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	switch op {
	case "b2_create_bucket":
		name := body["bucketName"].(string)
		if f.foreign[name] || f.buckets[name] != nil {
			fail(400, "duplicate_bucket_name")
			return
		}
		f.seq++
		body["bucketId"] = "b" + string(rune('0'+f.seq))
		f.buckets[name] = body
		json.NewEncoder(w).Encode(map[string]any{"bucketId": body["bucketId"], "bucketName": name})
	case "b2_list_buckets":
		var out []any
		if b := f.buckets[body["bucketName"].(string)]; b != nil {
			out = append(out, map[string]any{"bucketId": b["bucketId"], "bucketName": b["bucketName"],
				"fileLockConfiguration": map[string]any{"value": map[string]any{"isFileLockEnabled": b["fileLockEnabled"]}},
				"lifecycleRules":        b["lifecycleRules"]})
		}
		json.NewEncoder(w).Encode(map[string]any{"buckets": out})
	case "b2_delete_bucket":
		for name, b := range f.buckets {
			if b["bucketId"] == body["bucketId"] {
				delete(f.buckets, name)
				f.deleted = append(f.deleted, "bucket:"+name)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{})
	case "b2_create_key":
		var caps []string
		for _, c := range body["capabilities"].([]any) {
			caps = append(caps, c.(string))
		}
		f.seq++
		id := "minted-" + string(rune('0'+f.seq))
		f.keys[id] = caps
		del := slices.Contains(caps, "deleteFiles")
		f.s3.AddKey(id, fakes3.Rights{CanDeleteVersions: del, DenyHide: f.hideNeedsDelete && !del})
		json.NewEncoder(w).Encode(map[string]any{"applicationKeyId": id, "applicationKey": "secret-" + id, "keyName": body["keyName"]})
	case "b2_delete_key":
		id := body["applicationKeyId"].(string)
		delete(f.keys, id)
		f.deleted = append(f.deleted, "key:"+id)
		json.NewEncoder(w).Encode(map[string]any{})
	default:
		fail(400, "bad_request")
	}
}

func setupHarness(t *testing.T) (*harness, *fakeB2) {
	fake := fakes3.New("fog-house")
	fake.Lock = fakes3.LockConfig{Enabled: true}
	h := newHarness(t, fake)
	fake.AccessKey = "nobody" // only minted keys get in
	b2 := newFakeB2(t, fake, h.url)
	h.m.B2AuthURL = b2.srv.URL
	h.m.B2Settle = 1
	return h, b2
}

func setupReq() B2SetupRequest {
	return B2SetupRequest{Name: "backblaze", KeyID: "master-id", Secret: "master-secret", Bucket: "fog-house", ObjectLock: true, PricePerTB: 6.95}
}

func TestSetupB2MakesAnImmutableTarget(t *testing.T) {
	t.Parallel()

	h, b2 := setupHarness(t)
	rep, probe, err := h.m.SetupB2(context.Background(), setupReq())
	if err != nil {
		t.Fatalf("setup: %v (probe %+v)", err, probe)
	}
	if probe.Grade != "A" || rep.Adopted || !rep.ObjectLock || rep.DeleteFallback {
		t.Fatalf("grade %s, report %+v, steps %+v", probe.Grade, rep, probe.Steps)
	}
	b := b2.buckets["fog-house"]
	if b["fileLockEnabled"] != true || b["bucketType"] != "allPrivate" {
		t.Fatalf("bucket created as %v", b)
	}
	rules := b["lifecycleRules"].([]any)
	if days := rules[0].(map[string]any)["daysFromHidingToDeleting"].(float64); int(days) != rep.LifecycleDays || days < 14 {
		t.Fatalf("lifecycle %v, report %d", days, rep.LifecycleDays)
	}
	if len(b2.keys) != 1 {
		t.Fatalf("keys %v", b2.keys)
	}
	for id, caps := range b2.keys {
		if slices.Contains(caps, "deleteFiles") || slices.Contains(caps, "writeKeys") || slices.Contains(caps, "writeBuckets") {
			t.Fatalf("minted key holds %v", caps)
		}
		// The target holds the minted key — never the master.
		target, err := h.st.OffsiteTargetByName("backblaze")
		if err != nil {
			t.Fatal(err)
		}
		keyID, secret, err := h.m.credentials(target.Name)
		if err != nil || keyID != id || secret != "secret-"+id {
			t.Fatalf("stored %q/%q, %v", keyID, secret, err)
		}
		if target.Provider != "b2" || target.Bucket != "fog-house" || target.Grade != "A" {
			t.Fatalf("target %+v", target)
		}
	}
}

func TestSetupB2FallsBackWhenHidingNeedsDelete(t *testing.T) {
	t.Parallel()

	h, b2 := setupHarness(t)
	b2.hideNeedsDelete = true
	rep, probe, err := h.m.SetupB2(context.Background(), setupReq())
	if err != nil {
		t.Fatalf("setup: %v (probe %+v)", err, probe)
	}
	if !rep.DeleteFallback || !slices.Contains(rep.Capabilities, "deleteFiles") {
		t.Fatalf("report %+v", rep)
	}
	// The lock holds, so the wider key still grades A; the narrow key is gone.
	if probe.Grade != "A" || !probe.LockHolds {
		t.Fatalf("grade %s lockHolds %v: %+v", probe.Grade, probe.LockHolds, probe.Steps)
	}
	if len(b2.keys) != 1 || len(b2.deleted) != 1 || !strings.HasPrefix(b2.deleted[0], "key:") {
		t.Fatalf("keys %v, deleted %v", b2.keys, b2.deleted)
	}
}

func TestSetupB2Refusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		shape func(*fakeB2, *B2SetupRequest)
		want  string
	}{
		{"wrong key", func(_ *fakeB2, r *B2SetupRequest) { r.Secret = "nope" }, "did not accept"},
		{"scoped key", func(f *fakeB2, _ *B2SetupRequest) { f.scoped = true }, "restricted to a bucket"},
		{"weak key", func(f *fakeB2, _ *B2SetupRequest) { f.caps = []string{"listBuckets", "readFiles"} }, "writeBuckets, writeKeys"},
		{"name taken elsewhere", func(f *fakeB2, _ *B2SetupRequest) { f.foreign["fog-house"] = true }, "another Backblaze account"},
		{"bad bucket name", func(_ *fakeB2, r *B2SetupRequest) { r.Bucket = "b2-x" + strings.Repeat("y", 3) }, "bucket:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, b2 := setupHarness(t)
			req := setupReq()
			tc.shape(b2, &req)
			_, _, err := h.m.SetupB2(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "master-secret") {
				t.Fatal("the master key leaked into an error")
			}
			if len(b2.keys) != 0 {
				t.Fatalf("left keys behind: %v", b2.keys)
			}
		})
	}
}

func TestSetupB2UndoesAFailedProbe(t *testing.T) {
	t.Parallel()

	h, b2 := setupHarness(t)
	req := setupReq()
	req.Bucket = "fog-elsewhere" // B2 makes it; the S3 side never finds it
	if _, _, err := h.m.SetupB2(context.Background(), req); err == nil {
		t.Fatal("setup succeeded against a bucket S3 cannot reach")
	}
	if len(b2.keys) != 0 || len(b2.buckets) != 0 {
		t.Fatalf("left behind keys %v, buckets %v", b2.keys, b2.buckets)
	}
	if _, err := h.st.OffsiteTargetByName("backblaze"); err == nil {
		t.Fatal("a target was stored")
	}
}

func TestSetupB2RetryAdoptsItsBucket(t *testing.T) {
	t.Parallel()

	h, b2 := setupHarness(t)
	if _, _, err := h.m.SetupB2(context.Background(), setupReq()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.m.SetupB2(context.Background(), setupReq()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err %v", err)
	}
	// Another target on the same bucket adopts it rather than failing.
	req := setupReq()
	req.Name = "second"
	rep, probe, err := h.m.SetupB2(context.Background(), req)
	if err != nil {
		t.Fatalf("adopt: %v (probe %+v)", err, probe)
	}
	if !rep.Adopted || !rep.ObjectLock || rep.LifecycleDays < 14 {
		t.Fatalf("report %+v", rep)
	}
	if len(b2.keys) != 2 {
		t.Fatalf("keys %v", b2.keys)
	}
}
