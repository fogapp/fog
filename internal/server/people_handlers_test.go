package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// One login, one cookie: a person's session arrives under the same name the
// operator's does.
func personCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		// A sign-in's session, or a browser chat device's own credential
		// (signin_handlers_device.go).
		if (c.Name == auth.CookieName || c.Name == "fog_chat") && c.MaxAge >= 0 {
			return c
		}
	}
	t.Fatalf("no %s cookie in response: %s", auth.CookieName, w.Body)
	return nil
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return m
}

// inviteAndJoin mints an invite as the operator and joins as a member,
// returning the member's session cookie.
func inviteAndJoin(t *testing.T, h http.Handler, admin *http.Cookie, handle, password string) *http.Cookie {
	t.Helper()
	w := do(t, h, "POST", "/api/people/invites", `{"role":"member"}`, withCookie(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", w.Code, w.Body)
	}
	path := decode(t, w)["path"].(string)
	body := `{"handle":"` + handle + `","displayName":"` + strings.ToUpper(handle[:1]) + handle[1:] + `","password":"` + password + `"}`
	w = do(t, h, "POST", "/api"+path, body)
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	return personCookie(t, w)
}

func TestOperatorIsPersonOne(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)

	w := do(t, h, "GET", "/api/people", "", withCookie(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	got := decode(t, w)
	list := got["people"].([]any)
	if len(list) != 1 {
		t.Fatalf("want 1 person, got %d", len(list))
	}
	p := list[0].(map[string]any)
	if p["handle"] != "admin" || p["role"] != "operator" {
		t.Errorf("operator row: %+v", p)
	}

	// The operator's own session resolves as a person too.
	w = do(t, h, "GET", "/api/chat/me", "", withCookie(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("chat/me as operator: %d %s", w.Code, w.Body)
	}
	if me := decode(t, w); me["operator"] != true {
		t.Errorf("operator flag missing: %v", me)
	}

	// Members cannot administer people.
	member := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	if w := do(t, h, "GET", "/api/people", "", withCookie(member)); w.Code != http.StatusUnauthorized {
		t.Errorf("member listing people: %d", w.Code)
	}
}

func TestInviteJoinLogin(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)

	// Preview a bad invite: not found, and the same for expired/used.
	if w := do(t, h, "GET", "/api/join/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("bad invite preview: %d", w.Code)
	}

	w := do(t, h, "POST", "/api/people/invites", `{"handle":"Dave","displayName":"Dave","role":"member"}`, withCookie(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", w.Code, w.Body)
	}
	inv := decode(t, w)
	path := inv["path"].(string)
	api := "/api" + path

	w = do(t, h, "GET", api, "")
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	if pv := decode(t, w); pv["handle"] != "dave" {
		t.Errorf("preview handle: %v", pv["handle"])
	}

	// The invite pins the handle.
	if w := do(t, h, "POST", api, `{"handle":"eve","password":"correct horse battery"}`); w.Code != http.StatusBadRequest {
		t.Errorf("wrong handle accepted: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "POST", api, `{"password":"correct horse battery"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	c := personCookie(t, w)

	// Single use.
	if w := do(t, h, "POST", api, `{"password":"correct horse battery"}`); w.Code != http.StatusNotFound {
		t.Errorf("invite reused: %d", w.Code)
	}

	// The fresh session works, and so does a password login afterwards.
	if w := do(t, h, "GET", "/api/people/me", "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/people/login", `{"handle":"DAVE","password":"wrong"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("bad password: %d", w.Code)
	}
	w = do(t, h, "POST", "/api/people/login", `{"handle":"DAVE","password":"correct horse battery"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	c2 := personCookie(t, w)

	// Suspension ends sessions and blocks login.
	list := decode(t, do(t, h, "GET", "/api/people", "", withCookie(admin)))["people"].([]any)
	var daveID float64
	for _, p := range list {
		if p.(map[string]any)["handle"] == "dave" {
			daveID = p.(map[string]any)["id"].(float64)
		}
	}
	w = do(t, h, "PATCH", "/api/people/"+itoa(int(daveID)), `{"status":"suspended"}`, withCookie(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/people/me", "", withCookie(c2)); w.Code != http.StatusUnauthorized {
		t.Errorf("suspended session still valid: %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/people/login", `{"handle":"dave","password":"correct horse battery"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("suspended login: %d", w.Code)
	}
	// The operator cannot be suspended or removed.
	if w := do(t, h, "PATCH", "/api/people/1", `{"status":"suspended"}`, withCookie(admin)); w.Code != http.StatusBadRequest {
		t.Errorf("suspending operator: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/people/1", "", withCookie(admin)); w.Code != http.StatusBadRequest {
		t.Errorf("removing operator: %d", w.Code)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestIdentityDevicesAndKeys(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	dave := inviteAndJoin(t, h, admin, "dave", "correct horse battery")

	idPub, idPriv, _ := ed25519.GenerateKey(rand.Reader)
	devPub, _, _ := ed25519.GenerateKey(rand.Reader)
	enc := people.EncodeKey

	// A device attested before the identity exists is refused.
	att := ed25519.Sign(idPriv, people.AttestationMessage(devPub))
	w := do(t, h, "POST", "/api/chat/me/devices", `{"name":"Phone","deviceKey":"`+enc(devPub)+`","attestation":"`+enc(att)+`"}`, withCookie(dave))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("attest without identity: %d %s", w.Code, w.Body)
	}

	// Identity is write-once.
	if w := do(t, h, "PUT", "/api/chat/me/identity", `{"identityKey":"`+enc(idPub)+`"}`, withCookie(dave)); w.Code != http.StatusOK {
		t.Fatalf("set identity: %d %s", w.Code, w.Body)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if w := do(t, h, "PUT", "/api/chat/me/identity", `{"identityKey":"`+enc(other)+`"}`, withCookie(dave)); w.Code != http.StatusConflict {
		t.Errorf("second identity write: %d", w.Code)
	}

	// A forged attestation is refused; a real one is stored and the
	// registering session is bound to the device.
	bad := ed25519.Sign(idPriv, []byte("not the message"))
	if w := do(t, h, "POST", "/api/chat/me/devices", `{"deviceKey":"`+enc(devPub)+`","attestation":"`+enc(bad)+`"}`, withCookie(dave)); w.Code != http.StatusBadRequest {
		t.Errorf("forged attestation: %d %s", w.Code, w.Body)
	}
	kp := enc([]byte("0123456789abcdef-keypackage-1"))
	w = do(t, h, "POST", "/api/chat/me/devices",
		`{"name":"Phone","kind":"ios","deviceKey":"`+enc(devPub)+`","attestation":"`+enc(att)+`","keyPackages":["`+kp+`"]}`, withCookie(dave))
	if w.Code != http.StatusOK {
		t.Fatalf("register device: %d %s", w.Code, w.Body)
	}
	devCookie := personCookie(t, w)
	dev := decode(t, w)["device"].(map[string]any)
	if dev["verified"] != true || dev["keyPackages"].(float64) != 1 {
		t.Errorf("device view: %v", dev)
	}
	me := decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(devCookie)))
	if me["deviceId"].(float64) != dev["id"].(float64) {
		t.Errorf("session not bound to device: %v", me["deviceId"])
	}

	// An unattested second device is allowed but unverified; attesting it
	// from the identity key flips it.
	dev2Pub, _, _ := ed25519.GenerateKey(rand.Reader)
	w = do(t, h, "POST", "/api/chat/me/devices", `{"name":"Laptop","deviceKey":"`+enc(dev2Pub)+`"}`, withCookie(dave))
	if w.Code != http.StatusOK {
		t.Fatalf("register unattested: %d %s", w.Code, w.Body)
	}
	d2 := decode(t, w)["device"].(map[string]any)
	if d2["verified"] != false {
		t.Errorf("unattested device shown verified")
	}
	att2 := ed25519.Sign(idPriv, people.AttestationMessage(dev2Pub))
	w = do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(d2["id"].(float64)))+"/attest", `{"attestation":"`+enc(att2)+`"}`, withCookie(dave))
	if w.Code != http.StatusOK {
		t.Fatalf("attest: %d %s", w.Code, w.Body)
	}
	if decode(t, w)["device"].(map[string]any)["verified"] != true {
		t.Errorf("attested device not verified")
	}

	// Key material consumes one package per device; the second read finds
	// the phone empty and says so with null rather than an error.
	w = do(t, h, "GET", "/api/chat/people/dave/keys", "", withCookie(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("keys: %d %s", w.Code, w.Body)
	}
	km := decode(t, w)
	if km["identityKey"] != enc(idPub) {
		t.Errorf("identity key in key material: %v", km["identityKey"])
	}
	devs := km["devices"].([]any)
	if len(devs) != 2 {
		t.Fatalf("want 2 devices, got %d", len(devs))
	}
	if devs[0].(map[string]any)["keyPackage"] != kp {
		t.Errorf("phone key package: %v", devs[0].(map[string]any)["keyPackage"])
	}
	if devs[1].(map[string]any)["keyPackage"] != nil {
		t.Errorf("laptop should have no key package")
	}
	km = decode(t, do(t, h, "GET", "/api/chat/people/dave/keys", "", withCookie(admin)))
	if km["devices"].([]any)[0].(map[string]any)["keyPackage"] != nil {
		t.Errorf("key package handed out twice")
	}

	// Revoking the phone ends its session.
	w = do(t, h, "DELETE", "/api/chat/me/devices/"+itoa(int(dev["id"].(float64))), "", withCookie(dave))
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/chat/me", "", withCookie(devCookie)); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked device session still valid: %d", w.Code)
	}
	// The directory no longer lists it.
	dir := decode(t, do(t, h, "GET", "/api/chat/people", "", withCookie(admin)))
	for _, p := range dir["people"].([]any) {
		pm := p.(map[string]any)
		if pm["handle"] == "dave" && len(pm["devices"].([]any)) != 1 {
			t.Errorf("revoked device still in directory: %v", pm["devices"])
		}
	}

	// Operator-assisted identity reset clears the key and every device.
	if w := do(t, h, "POST", "/api/people/2/reset-identity", "", withCookie(admin)); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	me = decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(dave)))
	if me["identityKey"] != "" || len(me["devices"].([]any)) != 0 {
		t.Errorf("reset left state behind: %v %v", me["identityKey"], me["devices"])
	}
	if w := do(t, h, "PUT", "/api/chat/me/identity", `{"identityKey":"`+enc(other)+`"}`, withCookie(dave)); w.Code != http.StatusOK {
		t.Errorf("identity after reset: %d %s", w.Code, w.Body)
	}
}

func TestBackupRoundTrip(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	dave := inviteAndJoin(t, h, admin, "dave", "correct horse battery")

	if w := do(t, h, "GET", "/api/chat/me/backup", "", withCookie(dave)); w.Code != http.StatusNotFound {
		t.Errorf("empty backup: %d", w.Code)
	}
	put := func(body string, version string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/chat/me/backup", strings.NewReader(body))
		r.Header.Set("X-Fog-Backup-Version", version)
		r.AddCookie(dave)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := put("ciphertext-v2", "2"); w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := put("ciphertext-v1", "1"); w.Code != http.StatusConflict {
		t.Errorf("older version accepted: %d", w.Code)
	}
	w := do(t, h, "GET", "/api/chat/me/backup", "", withCookie(dave))
	if w.Code != http.StatusOK || w.Body.String() != "ciphertext-v2" || w.Header().Get("X-Fog-Backup-Version") != "2" {
		t.Errorf("get backup: %d %q %q", w.Code, w.Body, w.Header().Get("X-Fog-Backup-Version"))
	}
	// Another person cannot read it.
	eve := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	if w := do(t, h, "GET", "/api/chat/me/backup", "", withCookie(eve)); w.Code != http.StatusNotFound {
		t.Errorf("eve sees dave's backup: %d", w.Code)
	}
}

func TestPasskeysNeedSecureContext(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	admin := setup(t, h)
	// httptest requests are plain http on example.com: passkeys unavailable,
	// and the API says so with a distinct status rather than a 500.
	w := do(t, h, "POST", "/api/people/me/passkeys/begin", "", withCookie(admin))
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("passkey begin over http: %d %s", w.Code, w.Body)
	}
	me := decode(t, do(t, h, "GET", "/api/people/me", "", withCookie(admin)))
	if me["passkeysAvailable"] != false {
		t.Errorf("passkeysAvailable over http: %v", me["passkeysAvailable"])
	}
	// Over https with a forwarded host the ceremony starts.
	r := httptest.NewRequest("POST", "/api/people/me/passkeys/begin", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Host = "fog.example.com"
	r.AddCookie(hostCookie(admin)) // over https fog reads only __Host-fog_session
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("passkey begin over https: %d %s", rec.Code, rec.Body)
	}
	if got := decode(t, rec); got["ceremony"] == "" || got["options"] == nil {
		t.Errorf("begin response: %v", got)
	}
}

// Starting a passkey sign-in needs no account and holds a challenge for
// minutes, so it is bounded per address — in the passkey bucket, apart from
// passwords (signin_handlers_limits.go).
func TestPasskeyLoginBeginIsRateLimited(t *testing.T) {

	_, h := newTestServer(t)
	setup(t, h)
	for i := range passkeyAttempts {
		if w := do(t, h, "POST", "/api/people/login/passkey/begin", ""); w.Code == http.StatusTooManyRequests {
			t.Fatalf("begin %d limited early", i+1)
		}
	}
	if w := do(t, h, "POST", "/api/people/login/passkey/begin", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("begin past the limit: %d %s", w.Code, w.Body)
	}
	// Its own bucket: a flood of begins doesn't use up password sign-ins.
	if w := do(t, h, "POST", "/api/people/login", `{"handle":"nobody","password":"wrong"}`); w.Code == http.StatusTooManyRequests {
		t.Errorf("password login limited by passkey begins: %d", w.Code)
	}
}

// A person changing their own password or removing a credential leaves the
// same trail in the audit log as the operator doing it for them.
func TestSelfServiceCredentialChangesAreAudited(t *testing.T) {

	srv, h := newTestServer(t)
	admin := setup(t, h)
	c := inviteAndJoin(t, h, admin, "erin", "correct horse battery")
	lastAudit := func() store.AuditEntry {
		t.Helper()
		got, err := srv.store.RecentAudit(1)
		if err != nil || len(got) == 0 {
			t.Fatalf("audit: %v %v", got, err)
		}
		return got[0]
	}

	if w := do(t, h, "POST", "/api/people/me/password", `{"current":"correct horse battery","password":"another long phrase"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("set own password: %d %s", w.Code, w.Body)
	}
	if a := lastAudit(); a.Action != "people.password.set" || a.Subject != "erin" || !strings.Contains(a.Detail, `"self":true`) {
		t.Errorf("password change audit: %+v", a)
	}

	creds := decode(t, do(t, h, "GET", "/api/people/me", "", withCookie(c)))["credentials"].([]any)
	if len(creds) != 1 {
		t.Fatalf("credentials: %v", creds)
	}
	id := itoa(int(creds[0].(map[string]any)["id"].(float64)))
	// The only way in stays: removing it would lock erin out.
	if w := do(t, h, "DELETE", "/api/people/me/credentials/"+id, "", withCookie(c)); w.Code != http.StatusConflict {
		t.Fatalf("delete the only credential: %d %s", w.Code, w.Body)
	}
	erin, err := srv.store.PersonByHandle("erin")
	if err != nil {
		t.Fatal(err)
	}
	passkey, err := srv.store.AddPersonCredential(erin.ID, store.CredentialPasskey, "{}", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "DELETE", "/api/people/me/credentials/"+id, "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("delete own credential: %d %s", w.Code, w.Body)
	}
	a := lastAudit()
	if a.Action != "people.credential.remove" || a.Subject != "erin" || !strings.Contains(a.Detail, `"kind":"password"`) {
		t.Errorf("credential removal audit: %+v", a)
	}
	// Nothing to remove: still ok, and nothing recorded.
	if w := do(t, h, "DELETE", "/api/people/me/credentials/"+id, "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("delete again: %d %s", w.Code, w.Body)
	}
	if again := lastAudit(); again.ID != a.ID {
		t.Errorf("a no-op delete was audited: %+v", again)
	}
	if w := do(t, h, "DELETE", "/api/people/me/credentials/"+itoa(int(passkey)), "", withCookie(c)); w.Code != http.StatusConflict {
		t.Fatalf("delete the passkey that is now the only way in: %d %s", w.Code, w.Body)
	}
}
