package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

func pngBytes(side int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for x := 0; x < side; x++ {
		for y := 0; y < side; y++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// A previews subscription lists everything and holds nothing until opened
// or pinned; thumbnails come from the origin; the detail switch fills or
// frees; a listing share never yields bytes.
func TestFilesFederationPreviews(t *testing.T) {
	p := twoFogs(t, `[{"capability":"files.share"}]`)
	rootA, rootB := p.a.files.Root(), p.b.files.Root()
	os.MkdirAll(filepath.Join(rootA, "Pics"), 0o755)
	os.WriteFile(filepath.Join(rootA, "Pics", "dot.png"), pngBytes(8, color.RGBA{200, 30, 30, 255}), 0o644)
	os.WriteFile(filepath.Join(rootA, "Pics", "notes.txt"), []byte("hello"), 0o644)
	w := do(t, p.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Pics","title":"Pics"}`, p.bPeerOnA.ID), withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", w.Code, w.Body)
	}
	var sh filesfed.ShareView
	json.Unmarshal(w.Body.Bytes(), &sh)

	w = do(t, p.bH, "POST", "/api/files/subscriptions", fmt.Sprintf(`{"peerId":%d,"remoteShareId":%d,"detail":"previews"}`, p.aPeerOnB.ID, sh.ID), withCookie(p.bCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	var sub store.FileFedSubscription
	json.Unmarshal(w.Body.Bytes(), &sub)
	sync := func() {
		t.Helper()
		waitFor(t, 10*time.Second, func() bool {
			err := p.b.filesFed.Sync(context.Background(), sub.ID, nil)
			return err == nil
		})
	}
	sync()
	folder := filepath.Join(rootB, "Peers", "fog", "Pics")
	onDisk := func(name string) bool { _, err := os.Stat(filepath.Join(folder, name)); return err == nil }

	// Listed, not held.
	w = do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Pics", "", withCookie(p.bCookie))
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), `"dehydrated":true`) != 2 || !strings.Contains(w.Body.String(), `"name":"notes.txt"`) {
		t.Fatalf("previews listing: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"detail":"previews"`) || !strings.Contains(w.Body.String(), `"hydrated":false`) {
		t.Errorf("listing lacks hydration facts: %s", w.Body)
	}
	if onDisk("notes.txt") || onDisk("dot.png") {
		t.Fatal("previews subscription fetched bytes")
	}

	// Opening fetches.
	w = do(t, p.bH, "GET", "/api/files/raw/Peers/fog/Pics/notes.txt", "", withCookie(p.bCookie))
	if w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("open dehydrated: %d %q", w.Code, w.Body)
	}
	if !onDisk("notes.txt") {
		t.Fatal("open did not hydrate")
	}
	w = do(t, p.bH, "GET", "/api/files/stat?path=Peers/fog/Pics/notes.txt", "", withCookie(p.bCookie))
	if strings.Contains(w.Body.String(), `"dehydrated":true`) || !strings.Contains(w.Body.String(), `"hydrated":true`) {
		t.Errorf("stat after hydrate: %s", w.Body)
	}

	// The thumbnail comes from the origin for a file not held here.
	w = do(t, p.bH, "GET", "/api/files/thumb/Peers/fog/Pics/dot.png?w=64", "", withCookie(p.bCookie))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/jpeg" || w.Body.Len() == 0 {
		t.Fatalf("origin thumbnail: %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if onDisk("dot.png") {
		t.Fatal("a thumbnail must not hydrate the file")
	}
	// Cached now: the origin going away does not matter for it.
	w = do(t, p.bH, "GET", "/api/files/thumb/Peers/fog/Pics/dot.png?w=64", "", withCookie(p.bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("cached thumbnail: %d", w.Code)
	}

	// Keep offline pins and fetches.
	w = do(t, p.bH, "POST", "/api/files/hydrate", `{"path":"Peers/fog/Pics/dot.png","keep":true}`, withCookie(p.bCookie))
	if w.Code != http.StatusOK || !onDisk("dot.png") || !strings.Contains(w.Body.String(), `"pinned":true`) {
		t.Fatalf("keep offline: %d %s", w.Code, w.Body)
	}

	// The origin rewrites both: the pinned one is refetched, the on-demand
	// copy is listed again and its stale bytes go.
	os.WriteFile(filepath.Join(rootA, "Pics", "dot.png"), pngBytes(16, color.RGBA{30, 200, 30, 255}), 0o644)
	os.WriteFile(filepath.Join(rootA, "Pics", "notes.txt"), []byte("hello again"), 0o644)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(rootA, "Pics", "dot.png"), future, future)
	os.Chtimes(filepath.Join(rootA, "Pics", "notes.txt"), future, future)
	do(t, p.aH, "GET", "/api/files/ls?path=Pics", "", withCookie(p.aCookie))
	sync()
	if onDisk("notes.txt") {
		t.Error("a changed on-demand copy should be listed only")
	}
	if b, _ := os.ReadFile(filepath.Join(folder, "dot.png")); !bytes.Equal(b, pngBytes(16, color.RGBA{30, 200, 30, 255})) {
		t.Error("the pinned copy was not refetched")
	}
	w = do(t, p.bH, "GET", "/api/files/raw/Peers/fog/Pics/notes.txt", "", withCookie(p.bCookie))
	if w.Body.String() != "hello again" {
		t.Errorf("reopen after change: %d %q", w.Code, w.Body)
	}

	// Free up space on the pinned one.
	w = do(t, p.bH, "POST", "/api/files/hydrate", `{"path":"Peers/fog/Pics/dot.png","keep":false}`, withCookie(p.bCookie))
	if w.Code != http.StatusOK || onDisk("dot.png") || !strings.Contains(w.Body.String(), `"dehydrated":true`) {
		t.Fatalf("free up space: %d %s", w.Code, w.Body)
	}

	// Switch to a full copy: everything lands; back to previews: freed.
	w = do(t, p.bH, "PUT", fmt.Sprintf("/api/files/subscriptions/%d", sub.ID), `{"detail":"full"}`, withCookie(p.bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("detail full: %d %s", w.Code, w.Body)
	}
	waitFor(t, 10*time.Second, func() bool {
		w := do(t, p.bH, "GET", "/api/files/subscriptions", "", withCookie(p.bCookie))
		return strings.Contains(w.Body.String(), `"hydratedFiles":2`)
	})
	if !onDisk("dot.png") || !onDisk("notes.txt") {
		t.Fatal("full copy did not land")
	}
	w = do(t, p.bH, "PUT", fmt.Sprintf("/api/files/subscriptions/%d", sub.ID), `{"detail":"previews"}`, withCookie(p.bCookie))
	if w.Code != http.StatusOK || onDisk("dot.png") || onDisk("notes.txt") {
		t.Fatalf("detail previews: %d %s", w.Code, w.Body)
	}
	w = do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Pics", "", withCookie(p.bCookie))
	if strings.Count(w.Body.String(), `"dehydrated":true`) != 2 {
		t.Errorf("after switch back: %s", w.Body)
	}
	// The origin's ceiling holds.
	w = do(t, p.bH, "PUT", fmt.Sprintf("/api/files/subscriptions/%d", sub.ID), `{"detail":"contribute"}`, withCookie(p.bCookie))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad detail: %d", w.Code)
	}

	// A listing share: names cross, bytes never.
	os.MkdirAll(filepath.Join(rootA, "Docs"), 0o755)
	os.WriteFile(filepath.Join(rootA, "Docs", "will.pdf"), []byte("%PDF"), 0o644)
	w = do(t, p.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Docs","title":"Docs","detail":"listing"}`, p.bPeerOnA.ID), withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("listing share: %d %s", w.Code, w.Body)
	}
	var sh2 filesfed.ShareView
	json.Unmarshal(w.Body.Bytes(), &sh2)
	w = do(t, p.bH, "POST", "/api/files/subscriptions", fmt.Sprintf(`{"peerId":%d,"remoteShareId":%d}`, p.aPeerOnB.ID, sh2.ID), withCookie(p.bCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("subscribe listing: %d %s", w.Code, w.Body)
	}
	var sub2 store.FileFedSubscription
	json.Unmarshal(w.Body.Bytes(), &sub2)
	waitFor(t, 10*time.Second, func() bool { return p.b.filesFed.Sync(context.Background(), sub2.ID, nil) == nil })
	w = do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Docs", "", withCookie(p.bCookie))
	if !strings.Contains(w.Body.String(), `"name":"will.pdf"`) {
		t.Fatalf("listing entries: %s", w.Body)
	}
	if w := do(t, p.bH, "GET", "/api/files/raw/Peers/fog/Docs/will.pdf", "", withCookie(p.bCookie)); w.Code != http.StatusForbidden {
		t.Errorf("listing bytes: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.bH, "POST", "/api/files/hydrate", `{"path":"Peers/fog/Docs/will.pdf","keep":true}`, withCookie(p.bCookie)); w.Code != http.StatusForbidden {
		t.Errorf("pin on a listing: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.bH, "PUT", fmt.Sprintf("/api/files/subscriptions/%d", sub2.ID), `{"detail":"full"}`, withCookie(p.bCookie)); w.Code != http.StatusBadRequest {
		t.Errorf("full on a listing: %d %s", w.Code, w.Body)
	}

	// The bandwidth cap is the use row's config.
	w = do(t, p.bH, "PUT", fmt.Sprintf("/api/federation/peers/%d", p.aPeerOnB.ID), `{"capabilities":[{"capability":"files.share","direction":"use","enabled":true,"config":{"bandwidthBps":4096}}]}`, withCookie(p.bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("set cap: %d %s", w.Code, w.Body)
	}
	if got := p.b.filesFed.PeerBandwidth(p.aPeerOnB.ID); got != 4096 {
		t.Errorf("bandwidth = %d", got)
	}
}
