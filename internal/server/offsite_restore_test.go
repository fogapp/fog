package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"time"

	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/scrypt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/offsite"
)

// plantLocker puts a locker with one kit into a fake bucket the way a
// running fog would have, and returns the bucket's URL.
func plantLocker(t *testing.T, passphrase string) (url string, prefix string) {
	return plantLockerWith(t, passphrase, func(plain []byte) []byte {
		sealed, err := backup.EncryptKit(plain, passphrase)
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}, true)
}

// plantLockerWith is plantLocker with the kit's sealing chosen by the test,
// and whether the kit carries its sealed kit-meta.json (current kits do).
func plantLockerWith(t *testing.T, passphrase string, seal func(plain []byte) []byte, withMeta bool) (url string, prefix string) {
	t.Helper()
	fake := fakes3.New("fog-test")
	fake.AccessKey = "key-1"
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := objstore.New(objstore.Config{Endpoint: srv.URL, Region: "x", Bucket: "fog-test", AccessKey: "key-1", SecretKey: "s", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	lk, _, err := offsite.CreateLocker("old-house", passphrase, nil)
	if err != nil {
		t.Fatal(err)
	}
	layout := offsite.Layout{Prefix: "backups", ID: lk.ID}
	raw, _ := lk.Marshal()
	ctx := context.Background()
	if _, err := client.PutBytes(ctx, layout.LockerJSON(), raw, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// A kit: a tar.gz of master.key + fog.db, sealed under the passphrase.
	var kit bytes.Buffer
	gz := gzip.NewWriter(&kit)
	tw := tar.NewWriter(gz)
	entries := map[string]string{"master.key": strings.Repeat("ab", 32), "fog.db": "sqlite bytes"}
	if withMeta {
		entries["kit-meta.json"] = `{"format":"fog-recovery-kit/1","stamp":"20260827-010000","fog":"old-house","created_at":"2026-08-27T01:00:00Z"}`
	}
	for name, data := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)})
		tw.Write([]byte(data))
	}
	tw.Close()
	gz.Close()
	sealed := seal(kit.Bytes())
	if _, err := client.PutBytes(ctx, layout.Kit("fog-recovery-kit-20260827-010000.enc"), sealed, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	return srv.URL, "backups"
}

func TestRestoreOffsiteDoor(t *testing.T) {
	// The fake bucket is on loopback, which the SSRF guard rightly refuses;
	// relax it here so the door's own behaviour is what this exercises.
	// TestRestoreOffsiteRefusesInternalEndpoint covers the guard itself.
	restoreEndpointGuard = func(string) error { return nil }
	savedDial := restoreDialGuard
	restoreDialGuard = nil // the fake bucket is on loopback
	t.Cleanup(func() { restoreEndpointGuard = publicStorageEndpoint; restoreDialGuard = savedDial })
	_, h := newTestServer(t)
	url, prefix := plantLocker(t, "correct horse battery")
	body := func(pass string, dry bool) string {
		return fmt.Sprintf(`{"endpoint":%q,"region":"x","bucket":"fog-test","prefix":%q,"pathStyle":true,"keyId":"key-1","secret":"s","passphrase":%q,"dryRun":%v}`, url, prefix, pass, dry)
	}
	// A look inside, with the right passphrase: the locker, its kit, its apps.
	w := do(t, h, "POST", "/api/setup/restore-offsite", body("correct horse battery", true))
	if w.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", w.Code, w.Body)
	}
	var pv struct {
		Name string
		Kits []struct{ Name string }
	}
	json.Unmarshal(w.Body.Bytes(), &pv)
	if pv.Name != "old-house" || len(pv.Kits) != 1 {
		t.Fatalf("preview: %s", w.Body)
	}
	// The wrong passphrase is refused before anything is touched.
	if w := do(t, h, "POST", "/api/setup/restore-offsite", body("wrong wrong wrong", true)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "passphrase") {
		t.Fatalf("wrong passphrase: %d %s", w.Code, w.Body)
	}
	// Bad credentials are a plain refusal too.
	bad := strings.Replace(body("correct horse battery", true), `"keyId":"key-1"`, `"keyId":"nope"`, 1)
	if w := do(t, h, "POST", "/api/setup/restore-offsite", bad); w.Code != http.StatusBadRequest {
		t.Fatalf("bad key: %d %s", w.Code, w.Body)
	}
	// Once setup has happened the door is closed for good.
	setup(t, h)
	if w := do(t, h, "POST", "/api/setup/restore-offsite", body("correct horse battery", true)); w.Code != http.StatusForbidden {
		t.Fatalf("after setup: %d", w.Code)
	}
}

// Before the first operator exists, restore-offsite fetches whatever endpoint
// it is handed — SSRF. The handler must refuse an internal address, with no
// rate-limit or setup state standing in the way of proving it.
func TestRestoreOffsiteRefusesInternalEndpoint(t *testing.T) {
	_, h := newTestServer(t) // real guard, not relaxed
	for _, endpoint := range []string{
		"http://127.0.0.1:1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:9000/",
	} {
		body := fmt.Sprintf(`{"endpoint":%q,"region":"x","bucket":"b","prefix":"p","pathStyle":true,"keyId":"k","secret":"s","passphrase":"correct horse battery","dryRun":true}`, endpoint)
		w := do(t, h, "POST", "/api/setup/restore-offsite", body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "public object-storage") {
			t.Fatalf("%s: expected an SSRF refusal, got %d %s", endpoint, w.Code, w.Body)
		}
	}
}

// With the URL check passed (here relaxed) but the dial-level guard real, the
// door's connection to an internal address is refused — and the answer is a
// generic message, never the raw dial error naming the internal host.
func TestRestoreOffsiteDoesNotReflectDialErrors(t *testing.T) {
	restoreEndpointGuard = func(string) error { return nil }
	t.Cleanup(func() { restoreEndpointGuard = publicStorageEndpoint })
	_, h := newTestServer(t)
	url, prefix := plantLocker(t, "correct horse battery")
	body := fmt.Sprintf(`{"endpoint":%q,"region":"x","bucket":"fog-test","prefix":%q,"pathStyle":true,"keyId":"key-1","secret":"s","passphrase":"correct horse battery","dryRun":true}`, url, prefix)
	w := do(t, h, "POST", "/api/setup/restore-offsite", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a loopback bucket must be refused at dial time: %d %s", w.Code, w.Body)
	}
	for _, leak := range []string{"127.0.0.1", "refusing", "dial", "connect"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("the reply reflects the dial error (%q): %s", leak, w.Body)
		}
	}
}

// restoreDialGuard pins every connection the recovery door makes to a public
// address — the backstop for a redirect or DNS rebinding the URL-level check
// (first host only) misses.
func TestRestoreDialGuard(t *testing.T) {
	for _, bad := range []string{"127.0.0.1:443", "10.1.2.3:443", "169.254.169.254:80", "[::1]:443", "192.168.1.5:9000", "100.64.0.1:443", "198.18.0.1:443", "0.1.2.3:443", "[64:ff9b::a00:1]:443", "[2002:a00:1::1]:443"} {
		if err := restoreDialGuard("tcp", bad); err == nil {
			t.Errorf("dial guard allowed a non-public address %s", bad)
		}
	}
	for _, ok := range []string{"8.8.8.8:443", "1.1.1.1:443"} {
		if err := restoreDialGuard("tcp", ok); err != nil {
			t.Errorf("dial guard refused a public address %s: %v", ok, err)
		}
	}
}

// publicStorageEndpoint is the SSRF guard's logic, checked without DNS.
func TestPublicStorageEndpoint(t *testing.T) {
	for _, bad := range []string{
		"http://127.0.0.1", "https://127.0.0.1:9000", "http://[::1]/",
		"http://169.254.169.254/", "https://10.1.2.3/", "http://192.168.0.9/",
		"https://172.16.5.5/", "http://0.0.0.0/", "ftp://example.com/", "not a url", "",
	} {
		if err := publicStorageEndpoint(bad); err == nil {
			t.Errorf("publicStorageEndpoint(%q) accepted an SSRF-capable endpoint", bad)
		}
	}
	for _, ok := range []string{"https://8.8.8.8/", "https://1.1.1.1", "http://93.184.216.34/"} {
		if err := publicStorageEndpoint(ok); err != nil {
			t.Errorf("publicStorageEndpoint(%q) rejected a public endpoint: %v", ok, err)
		}
	}
}

func TestOffsiteRestoreRoutesNeedAuth(t *testing.T) {
	_, h := newTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/apps/hub/offsite"}, {"POST", "/api/offsite/fetch"}, {"GET", "/api/offsite/files"},
		{"POST", "/api/offsite/files/restore"}, {"POST", "/api/offsite/recover"}, {"GET", "/api/offsite/jobs/x"},
	} {
		if w := do(t, h, tc.method, tc.path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
	c := setup(t, h)
	// With no targets the lists are simply empty.
	w := do(t, h, "GET", "/api/offsite/files", "", withCookie(c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"snapshots":[]`) {
		t.Fatalf("files: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/offsite/recover", `{"target":"nope"}`, withCookie(c)); w.Code != http.StatusNotFound {
		t.Fatalf("recover unknown target: %d %s", w.Code, w.Body)
	}
}

// legacySeal seals a kit the way an older fog did (FOGKIT1, scrypt).
func legacySeal(t *testing.T, plain []byte, passphrase string) []byte {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := scrypt.Key([]byte(passphrase), salt, 1<<15, 8, 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	out := append([]byte("FOGKIT1\x00"), salt...)
	out = append(out, nonce...)
	return append(out, gcm.Seal(nil, nonce, plain, []byte("FOGKIT1\x00"))...)
}

// A legacy kit (an older fog's) cannot prove its date: an old one copied to a
// newer name looks the same. The setup preview must say so before anything
// is restored, and the restore must refuse it without the operator's
// explicit go-ahead. A current-format kit without its sealed stamp is
// refused outright.
func TestRestoreOffsiteWarnsOnLegacyKit(t *testing.T) {
	restoreEndpointGuard = func(string) error { return nil }
	savedDial := restoreDialGuard
	restoreDialGuard = nil
	t.Cleanup(func() { restoreEndpointGuard = publicStorageEndpoint; restoreDialGuard = savedDial })
	const pass = "correct horse battery"
	body := func(url, prefix string, dry bool) string {
		return fmt.Sprintf(`{"endpoint":%q,"region":"x","bucket":"fog-test","prefix":%q,"pathStyle":true,"keyId":"key-1","secret":"s","passphrase":%q,"dryRun":%v}`, url, prefix, pass, dry)
	}

	_, h := newTestServer(t)
	url, prefix := plantLockerWith(t, pass, func(p []byte) []byte { return legacySeal(t, p, pass) }, false)
	w := do(t, h, "POST", "/api/setup/restore-offsite", body(url, prefix, true))
	var pv struct {
		NewestKit struct {
			Name    string
			Legacy  bool
			Date    string
			Warning string
			SHA256  string
		} `json:"newestKit"`
	}
	json.Unmarshal(w.Body.Bytes(), &pv)
	if w.Code != http.StatusOK || !pv.NewestKit.Legacy || pv.NewestKit.Warning == "" || pv.NewestKit.Date == "" {
		t.Fatalf("the preview must flag a legacy kit with its inside date: %d %s", w.Code, w.Body)
	}
	pinned := strings.TrimSuffix(body(url, prefix, false), "}") + fmt.Sprintf(`,"kit":{"name":%q,"sha256":%q}}`, pv.NewestKit.Name, pv.NewestKit.SHA256)
	if w := do(t, h, "POST", "/api/setup/restore-offsite", pinned); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "legacy_kit") {
		t.Fatalf("restoring a legacy kit without the go-ahead must be refused: %d %s", w.Code, w.Body)
	}
	// Consent is bound to the kit the preview showed: a go-ahead naming the
	// right kit but another digest (a different kit swapped in under the same
	// name) is refused like no go-ahead at all.
	wrong := strings.TrimSuffix(pinned, "}") + `,"acceptLegacyKit":{"name":"fog-recovery-kit-20260827-010000.enc","sha256":"0000"}}`
	if w := do(t, h, "POST", "/api/setup/restore-offsite", wrong); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "legacy_kit") {
		t.Fatalf("consent for a different kit must not carry over: %d %s", w.Code, w.Body)
	}
	// A restore that names no kit — not the preview's — is refused.
	if w := do(t, h, "POST", "/api/setup/restore-offsite", body(url, prefix, false)); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "kit_unconfirmed") {
		t.Fatalf("a restore without the preview's kit: %d %s", w.Code, w.Body)
	}
	// And one whose pinned digest no longer matches the bucket's bytes (a kit
	// swapped in after the preview) restores nothing.
	swapped := strings.TrimSuffix(body(url, prefix, false), "}") + fmt.Sprintf(`,"kit":{"name":%q,"sha256":%q}}`, pv.NewestKit.Name, strings.Repeat("0", 64))
	if w := do(t, h, "POST", "/api/setup/restore-offsite", swapped); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "kit_changed") {
		t.Fatalf("a kit changed since the preview: %d %s", w.Code, w.Body)
	}

	// A current-format kit stripped of its sealed stamp is refused.
	_, h2 := newTestServer(t)
	url2, prefix2 := plantLockerWith(t, pass, func(p []byte) []byte {
		sealed, _ := backup.EncryptKit(p, pass)
		return sealed
	}, false)
	w2 := do(t, h2, "POST", "/api/setup/restore-offsite", body(url2, prefix2, true))
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "sealed stamp") {
		t.Fatalf("the preview must say a FOGKIT2 kit without kit-meta.json is refused: %d %s", w2.Code, w2.Body)
	}
	var kits struct{ Kits []struct{ Name string } }
	json.Unmarshal(w2.Body.Bytes(), &kits)
	stripped := strings.TrimSuffix(body(url2, prefix2, false), "}") + fmt.Sprintf(`,"kit":{"name":%q,"sha256":%q}}`, kits.Kits[0].Name, strings.Repeat("a", 64))
	if w := do(t, h2, "POST", "/api/setup/restore-offsite", stripped); w.Code != http.StatusConflict {
		t.Fatalf("a FOGKIT2 kit without kit-meta.json must be refused: %d %s", w.Code, w.Body)
	}
}
