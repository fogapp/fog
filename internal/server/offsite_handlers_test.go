package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/offsite"
)

func init() {
	// Creating a locker derives a 256 MiB Argon2id key; the handler tests
	// are about the HTTP surface, not the KDF.
	offsite.DefaultKDF = offsite.KDFParams{Time: 1, Memory: 8 * 1024, Threads: 1}
}

func TestOffsiteTargetsAPI(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	fake := fakes3.New("fog-test")
	fake.AccessKey = "key-1"
	bucket := httptest.NewServer(fake)
	defer bucket.Close()
	srv.offsite.HTTPClient = bucket.Client()

	// Unauthenticated: nothing.
	if w := do(t, h, "GET", "/api/offsite/targets", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list: %d", w.Code)
	}
	body := fmt.Sprintf(`{"name":"cloud","provider":"b2","endpoint":%q,"region":"us-west-004","bucket":"fog-test","prefix":"backups","pathStyle":true,"pricePerTb":6.95,"keyId":"key-1","secret":"s"}`, bucket.URL)

	// No passphrase yet: a precondition, said as one.
	if w := do(t, h, "POST", "/api/offsite/targets", body, withCookie(c)); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "passphrase") {
		t.Fatalf("without passphrase: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/settings", `{"recovery_kit_passphrase":"correct horse battery"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("set passphrase: %d %s", w.Code, w.Body)
	}
	w := do(t, h, "POST", "/api/offsite/targets", body, withCookie(c))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		Target struct {
			Name, Grade, GradeLabel, LockerRoot string
			Probe                               string
		}
		Probe struct{ Fix string }
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Target.Grade != "B" || created.Target.GradeLabel != "Append-only" || created.Probe.Fix == "" || created.Target.Probe != "" {
		t.Fatalf("created: %+v", created)
	}
	if !strings.HasPrefix(created.Target.LockerRoot, "backups/fog-locker/1/") {
		t.Fatalf("locker root %q", created.Target.LockerRoot)
	}
	if strings.Contains(w.Body.String(), `"secret"`) || strings.Contains(w.Body.String(), "key-1") {
		t.Fatal("credentials leaked into the response")
	}

	// Presets carry the grade-A warning.
	w = do(t, h, "GET", "/api/offsite/presets", "", withCookie(c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "one-way door") {
		t.Fatalf("presets: %d %s", w.Code, w.Body)
	}

	// Listing shows the target with the parsed probe and a cost of zero.
	w = do(t, h, "GET", "/api/offsite/targets", "", withCookie(c))
	var list struct {
		Targets []struct {
			Name        string
			ProbeResult *struct{ Grade string }
			MonthlyCost float64
		}
		PassphraseSet bool
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Targets) != 1 || list.Targets[0].ProbeResult == nil || list.Targets[0].ProbeResult.Grade != "B" || !list.PassphraseSet {
		t.Fatalf("list: %s", w.Body)
	}

	// A bucket that cannot be used is the operator's input: 400 with the
	// steps that failed.
	bad := strings.Replace(body, `"name":"cloud"`, `"name":"other"`, 1)
	bad = strings.Replace(bad, `"keyId":"key-1"`, `"keyId":"nope"`, 1)
	if w := do(t, h, "POST", "/api/offsite/targets", bad, withCookie(c)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"probe"`) {
		t.Fatalf("bad key: %d %s", w.Code, w.Body)
	}

	// Re-probe after the operator turned on Object Lock.
	fake.Lock = fakes3.LockConfig{Enabled: true}
	w = do(t, h, "POST", "/api/offsite/targets/cloud/probe", "", withCookie(c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"grade":"A"`) {
		t.Fatalf("reprobe: %d %s", w.Code, w.Body)
	}

	if w := do(t, h, "DELETE", "/api/offsite/targets/cloud", "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "DELETE", "/api/offsite/targets/cloud", "", withCookie(c)); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", w.Code)
	}
}
