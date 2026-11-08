package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/feedback"
	"gitea.lan/matthew/fog/internal/logring"
)

func feedbackSendMode(t *testing.T, h http.Handler, cookie *http.Cookie, peerID int64, mode string) int {
	t.Helper()
	body := fmt.Sprintf(`{"capabilities":[{"capability":"feedback.ingest","direction":"use","enabled":true,"config":{"mode":%q}}]}`, mode)
	if mode == "" {
		body = `{"capabilities":[{"capability":"feedback.ingest","direction":"use","enabled":false}]}`
	}
	return do(t, h, "PUT", fmt.Sprintf("/api/federation/peers/%d", peerID), body, withCookie(cookie)).Code
}

type createdFeedback struct {
	Feedback feedbackView     `json:"feedback"`
	Attached feedbackAttached `json:"attached"`
}

func postFeedback(t *testing.T, h http.Handler, cookie *http.Cookie, body string) createdFeedback {
	t.Helper()
	w := do(t, h, "POST", "/api/feedback", body, withCookie(cookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("create feedback: %d %s", w.Code, w.Body)
	}
	var out createdFeedback
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// B's people write feedback; B sends it to A, which granted feedback.ingest
// in the invite. Both run the real handler stack, A over real HTTP.
func TestFeedbackRelaysToPeer(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[{"capability":"feedback.ingest"}]`)

	// B keeps a log tail, and something worth reading is in it.
	ring := logring.New(50)
	p.b.logRing = ring
	blog := slog.New(ring.Handler(slog.NewTextHandler(io.Discard, nil)))
	blog.Info("proxy reloaded")
	blog.Warn("deploy failed", "app", "manor", "err", "exit status 1")

	if code := feedbackSendMode(t, p.bH, p.bCookie, p.aPeerOnB.ID, "everything"); code != http.StatusBadRequest {
		t.Fatalf("an unknown mode should be refused, got %d", code)
	}
	if code := feedbackSendMode(t, p.bH, p.bCookie, p.aPeerOnB.ID, "full"); code != http.StatusOK {
		t.Fatalf("set mode: %d", code)
	}

	if w := do(t, p.bH, "POST", "/api/feedback", `{"body":"   "}`, withCookie(p.bCookie)); w.Code != http.StatusBadRequest {
		t.Fatalf("empty report: %d", w.Code)
	}

	got := postFeedback(t, p.bH, p.bCookie, `{
		"body":"Saving the backup schedule did nothing\nno error either",
		"page":{"path":"/apps/manor","search":"?tab=backups","title":"manor"},
		"client":{"userAgent":"test-browser","events":[{"kind":"request","message":"PUT /api/apps/manor/backup → 500"}]}
	}`)
	f := got.Feedback
	if f.Page != "/apps/manor?tab=backups" || f.Author == "" || f.Status != "open" || f.Dossier != "" {
		t.Fatalf("created = %+v", f)
	}
	if got.Attached.Logs != 2 || got.Attached.Warnings != 1 || !got.Attached.Browser {
		t.Fatalf("attached = %+v", got.Attached)
	}
	// Sent while the dialog waited: delivered, on A's signed word.
	if len(f.Deliveries) != 1 || f.Deliveries[0].State != "delivered" || f.Deliveries[0].Mode != "full" ||
		f.Deliveries[0].Peer != p.aPeerOnB.DisplayName() {
		t.Fatalf("deliveries = %+v", f.Deliveries)
	}

	// A has it in its inbox, attributed to B, with the dossier intact.
	w := do(t, p.aH, "GET", "/api/feedback?status=open", "", withCookie(p.aCookie))
	var list struct {
		Feedback []feedbackView `json:"feedback"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Feedback) != 1 || list.Feedback[0].Origin != p.bPeerOnA.DisplayName() || !list.Feedback[0].HasDossier ||
		list.Feedback[0].Dossier != "" {
		t.Fatalf("A's list = %+v", list.Feedback)
	}
	onA := list.Feedback[0]
	w = do(t, p.aH, "GET", fmt.Sprintf("/api/feedback/%d", onA.ID), "", withCookie(p.aCookie))
	var full feedbackView
	json.Unmarshal(w.Body.Bytes(), &full)
	var dossier feedback.Dossier
	if err := json.Unmarshal([]byte(full.Dossier), &dossier); err != nil {
		t.Fatalf("dossier: %v (%q)", err, full.Dossier)
	}
	if dossier.Page.Path != "/apps/manor" || len(dossier.Warnings) != 1 || dossier.Warnings[0].Msg != "deploy failed" ||
		!strings.Contains(string(dossier.Client), "→ 500") || dossier.Fog.Version == "" {
		t.Fatalf("dossier = %+v", dossier)
	}

	// The bell counts someone else's report; B's own is not a to-do there.
	var sum struct {
		Feedback struct{ Open, FromPeers int64 } `json:"feedback"`
	}
	json.Unmarshal(do(t, p.aH, "GET", "/api/inbox/summary", "", withCookie(p.aCookie)).Body.Bytes(), &sum)
	if sum.Feedback.Open != 1 || sum.Feedback.FromPeers != 1 {
		t.Fatalf("A summary = %+v", sum)
	}
	json.Unmarshal(do(t, p.bH, "GET", "/api/inbox/summary", "", withCookie(p.bCookie)).Body.Bytes(), &sum)
	if sum.Feedback.Open != 1 || sum.Feedback.FromPeers != 0 {
		t.Fatalf("B summary = %+v", sum)
	}

	// A retry of the same uid is accepted and changes nothing; an invalid
	// item is refused by name.
	batch, _ := json.Marshal(feedback.Batch{Items: []feedback.Item{
		{UID: f.UID, Body: "again", CreatedAt: f.CreatedAt},
		{UID: "bad", Body: "", CreatedAt: f.CreatedAt},
	}})
	raw, err := p.b.fed.SignedPostVerified(context.Background(), p.aPeerOnB, "/fed/v1/feedback", "application/json", batch)
	if err != nil {
		t.Fatal(err)
	}
	var ans feedback.Answer
	json.Unmarshal(raw, &ans)
	if len(ans.Accepted) != 1 || ans.Accepted[0] != f.UID || ans.Refused["bad"] == "" {
		t.Fatalf("answer = %+v", ans)
	}
	if all, _, _ := p.a.store.CountOpenFeedback(); all != 1 {
		t.Fatalf("retry duplicated the report: %d", all)
	}

	// Note mode sends the words and leaves the dossier home.
	feedbackSendMode(t, p.bH, p.bCookie, p.aPeerOnB.ID, "note")
	postFeedback(t, p.bH, p.bCookie, `{"body":"wish the photos app had albums","page":{"path":"/"}}`)
	json.Unmarshal(do(t, p.aH, "GET", "/api/feedback", "", withCookie(p.aCookie)).Body.Bytes(), &list)
	if len(list.Feedback) != 2 || list.Feedback[0].HasDossier {
		t.Fatalf("note mode = %+v", list.Feedback)
	}

	// A stops accepting: B's next report stays queued, saying why.
	do(t, p.aH, "PUT", fmt.Sprintf("/api/federation/peers/%d", p.bPeerOnA.ID),
		`{"capabilities":[{"capability":"feedback.ingest","direction":"grant","enabled":false}]}`, withCookie(p.aCookie))
	stuck := postFeedback(t, p.bH, p.bCookie, `{"body":"third","page":{"path":"/"}}`).Feedback
	if len(stuck.Deliveries) != 1 || stuck.Deliveries[0].State != "queued" ||
		!strings.Contains(stuck.Deliveries[0].LastError, "not granted") {
		t.Fatalf("refused delivery = %+v", stuck.Deliveries)
	}
	// B stops sending: the queued report never leaves, and says nothing about a peer.
	feedbackSendMode(t, p.bH, p.bCookie, p.aPeerOnB.ID, "")
	w = do(t, p.bH, "GET", fmt.Sprintf("/api/feedback/%d", stuck.ID), "", withCookie(p.bCookie))
	var after feedbackView
	json.Unmarshal(w.Body.Bytes(), &after)
	if len(after.Deliveries) != 0 {
		t.Fatalf("a withdrawn send should cancel the queue: %+v", after.Deliveries)
	}
	// With nowhere to send, a report is simply filed.
	if lone := postFeedback(t, p.bH, p.bCookie, `{"body":"fourth"}`).Feedback; len(lone.Deliveries) != 0 {
		t.Fatalf("no destinations = %+v", lone.Deliveries)
	}

	// Close, reopen, delete.
	id := fmt.Sprintf("/api/feedback/%d", onA.ID)
	if w := do(t, p.aH, "POST", id+"/status", `{"status":"closed"}`, withCookie(p.aCookie)); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"status":"closed"`) {
		t.Fatalf("close: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.aH, "POST", id+"/status", `{"status":"done"}`, withCookie(p.aCookie)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", w.Code)
	}
	if w := do(t, p.aH, "DELETE", id, "", withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := do(t, p.aH, "GET", id, "", withCookie(p.aCookie)); w.Code != http.StatusNotFound {
		t.Fatalf("deleted report: %d", w.Code)
	}
}

// A peer without the grant is turned away before anything is read.
func TestFedFeedbackNeedsGrant(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[]`)
	batch, _ := json.Marshal(feedback.Batch{Items: []feedback.Item{{UID: "x", Body: "hi", CreatedAt: "2026-09-10T10:00:00Z"}}})
	if _, err := p.b.fed.SignedPostVerified(context.Background(), p.aPeerOnB, "/fed/v1/feedback", "application/json", batch); err == nil {
		t.Fatal("an ungranted peer's report should be refused")
	}
	if all, _, _ := p.a.store.CountOpenFeedback(); all != 0 {
		t.Fatalf("stored %d reports from an ungranted peer", all)
	}
}

// A's word on B's report comes back to B: in A's own dial when it has B's
// address, and in the answer to B's dial otherwise. B's person is told,
// the row shows who resolved it, and a reopen on B is not overruled.
func TestFeedbackResolutionReachesOrigin(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[{"capability":"feedback.ingest"}]`)
	ctx := context.Background()
	feedbackSendMode(t, p.bH, p.bCookie, p.aPeerOnB.ID, "note")

	var pushes []string
	var mu sync.Mutex
	p.b.pushNotify = func(title, msg string, ok bool) {
		mu.Lock()
		defer mu.Unlock()
		pushes = append(pushes, title+": "+msg)
	}

	onB := postFeedback(t, p.bH, p.bCookie, `{"body":"the photos app forgets my albums\nevery time","page":{"path":"/apps/photos"}}`).Feedback
	if len(onB.Deliveries) != 1 || onB.Deliveries[0].State != "delivered" {
		t.Fatalf("deliveries = %+v", onB.Deliveries)
	}
	var list struct {
		Feedback []feedbackView `json:"feedback"`
	}
	json.Unmarshal(do(t, p.aH, "GET", "/api/feedback?status=open", "", withCookie(p.aCookie)).Body.Bytes(), &list)
	if len(list.Feedback) != 1 {
		t.Fatalf("A's list = %+v", list.Feedback)
	}
	onA := list.Feedback[0]
	readB := func() feedbackView {
		var v feedbackView
		json.Unmarshal(do(t, p.bH, "GET", fmt.Sprintf("/api/feedback/%d", onB.ID), "", withCookie(p.bCookie)).Body.Bytes(), &v)
		return v
	}

	// B is dial-in only so far: A closing says nothing to B until B dials.
	if w := do(t, p.aH, "POST", fmt.Sprintf("/api/feedback/%d/status", onA.ID), `{"status":"closed"}`, withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("close on A: %d %s", w.Code, w.Body)
	}
	if v := readB(); v.Status != "open" {
		t.Fatalf("B heard without dialing: %+v", v)
	}
	if err := p.b.fed.HeartbeatPeer(ctx, p.aPeerOnB); err != nil {
		t.Fatal(err)
	}
	v := readB()
	if v.Status != "closed" || v.StatusBy != p.aPeerOnB.DisplayName() || v.ClosedAt == "" || v.StatusAt != v.ClosedAt {
		t.Fatalf("B after dial = %+v", v)
	}
	mu.Lock()
	if len(pushes) != 1 || !strings.Contains(pushes[0], "resolved your report") || !strings.Contains(pushes[0], "forgets my albums") {
		t.Fatalf("pushes = %v", pushes)
	}
	mu.Unlock()
	var sum struct {
		Feedback struct{ Open int64 } `json:"feedback"`
	}
	json.Unmarshal(do(t, p.bH, "GET", "/api/inbox/summary", "", withCookie(p.bCookie)).Body.Bytes(), &sum)
	if sum.Feedback.Open != 0 {
		t.Fatalf("B still counts it open: %+v", sum)
	}

	// B's person reopens. The next dial carries A's same closure again, and
	// it does not overrule them.
	if w := do(t, p.bH, "POST", fmt.Sprintf("/api/feedback/%d/status", onB.ID), `{"status":"open"}`, withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("reopen on B: %d %s", w.Code, w.Body)
	}
	if err := p.b.fed.HeartbeatPeer(ctx, p.aPeerOnB); err != nil {
		t.Fatal(err)
	}
	if v := readB(); v.Status != "open" || v.StatusBy != "" {
		t.Fatalf("B's reopen overruled: %+v", v)
	}

	// Now B has an address and A knows it: A's own dial carries the word,
	// and closing on A kicks that dial at once.
	bHTTP := httptest.NewServer(p.bH)
	defer bHTTP.Close()
	do(t, p.bH, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, bHTTP.URL), withCookie(p.bCookie))
	if err := p.b.fed.HeartbeatPeer(ctx, p.aPeerOnB); err != nil {
		t.Fatal(err)
	}
	if bOnA, _ := p.a.store.FedPeerByID(p.bPeerOnA.ID); bOnA.URL != bHTTP.URL {
		t.Fatalf("A did not learn B's address: %+v", bOnA)
	}
	do(t, p.aH, "POST", fmt.Sprintf("/api/feedback/%d/status", onA.ID), `{"status":"open"}`, withCookie(p.aCookie))
	do(t, p.aH, "POST", fmt.Sprintf("/api/feedback/%d/status", onA.ID), `{"status":"closed"}`, withCookie(p.aCookie))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v := readB(); v.Status == "closed" && v.StatusBy == p.aPeerOnB.DisplayName() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("A's closure never reached B on A's own dial: %+v", readB())
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	n := len(pushes)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("pushes after second closure = %v", pushes)
	}
	// A reopening travels the same way, and the row says so.
	do(t, p.aH, "POST", fmt.Sprintf("/api/feedback/%d/status", onA.ID), `{"status":"open"}`, withCookie(p.aCookie))
	deadline = time.Now().Add(5 * time.Second)
	for {
		if v := readB(); v.Status == "open" && v.StatusBy == p.aPeerOnB.DisplayName() && v.ClosedAt == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("A's reopen never reached B: %+v", readB())
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	if len(pushes) != 3 || !strings.Contains(pushes[2], "reopened your report") {
		t.Fatalf("pushes after reopen = %v", pushes)
	}
	mu.Unlock()
}

// Feedback filed from a route that names no installed app is not a gap in
// the dossier: there is simply no app to describe.
func TestDossierAppMissingIsNotAGap(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t)
	d := &feedback.Dossier{}
	srv.dossierApp(context.Background(), d, "no-such-app")
	if d.App != nil || len(d.Gaps) != 0 {
		t.Fatalf("missing app: App=%v Gaps=%v", d.App, d.Gaps)
	}
}
