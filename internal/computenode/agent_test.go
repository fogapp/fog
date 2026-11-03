package computenode

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
	"gitea.lan/matthew/fog/internal/selfupdate"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParsesFFmpegListings(t *testing.T) {
	encoders := ` V....D h264_videotoolbox    VideoToolbox H.264 Encoder (codec h264)
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D mpeg4                MPEG-4 part 2
 A....D aac                  AAC (Advanced Audio Coding)
 V....D hevc_videotoolbox    VideoToolbox H.265 Encoder (codec hevc)`
	if got := strings.Join(videoEncoders(encoders), ","); got != "h264_videotoolbox,libx264,hevc_videotoolbox" {
		t.Fatalf("encoders: %s", got)
	}
	filters := ` ... scale_vt          V->V       Scale Videotoolbox frames
 .S tonemap           V->V       Conversion to/from different dynamic ranges.
 T. yadif_videotoolbox V->V       YADIF for VideoToolbox frames using Metal compute`
	if got := strings.Join(filterNames(filters), ","); got != "scale_vt,tonemap,yadif_videotoolbox" {
		t.Fatalf("filters: %s", got)
	}
}

// fakeSalt is the salt of the fake fog's live code.
const fakeSalt = "fedcba9876543210fedcba9876543210"

// fogSigner is the fog these tests' machines are paired with, and fogKey
// its key as they pin it.
var (
	fogSigner = compute.NewKeySigner()
	fogKey    = fogSigner.PublicKeyB64()
)

// fakeFog is fogd's /compute/v1 as far as the agent sees it: it takes a
// machine's MACed polls and signs its answers, as fogd does, with signer
// (fogSigner unless a test makes it someone else).
type fakeFog struct {
	mu      sync.Mutex
	reports []compute.NodeReport
	answer  compute.NodeAnswer
	status  int
	binary  []byte
	// hold keeps a poll that may wait open this long, as fogd does.
	hold time.Duration
	// signer signs answers; unsigned sends them without a signature.
	signer   *compute.KeySigner
	unsigned bool
	// sawToken: a request carried the machine's credential itself.
	sawToken bool
	// fetched counts downloads of a build.
	fetched int
	// code is the refusal's code when status is set (default: fog has no
	// such machine, for a 401).
	code string
	// raw answers every poll with this status and body, unsigned.
	raw *rawAnswer
	// digest is the credential digest the machine sealed at pairing, or
	// rotated to; until one pairs, the tests' fognode_x. prev is the one
	// before a rotation.
	digest, prev string
	// rotations counts rotations taken.
	rotations int
	// sawDigest: a request carried the credential's digest in the clear.
	sawDigest bool
}

type rawAnswer struct {
	status int
	body   string
}

func (f *fakeFog) sign(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, _ := json.Marshal(v)
	signer := fogSigner
	if f.signer != nil {
		signer = *f.signer
	}
	if nonce := r.Header.Get(compute.HeaderNodeNonce); nonce != "" && !f.unsigned {
		w.Header().Set(compute.HeaderFogSignature, signer.SignTranscript(compute.AnswerLabel, compute.AnswerNonce("n1", nonce), body))
	}
	w.WriteHeader(status)
	w.Write(body)
}

func (f *fakeFog) lastReport() compute.NodeReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reports[len(f.reports)-1]
}

func (f *fakeFog) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("Authorization"), "fognode_") || strings.Contains(string(body), "fognode_") {
			f.sawToken = true
		}
		digest := f.digest
		if digest == "" {
			digest = compute.TokenDigest("fognode_x")
		}
		for _, v := range r.Header {
			if strings.Contains(strings.Join(v, " "), digest) {
				f.sawDigest = true
			}
		}
		if strings.Contains(string(body), digest) {
			f.sawDigest = true
		}
		switch r.URL.Path {
		case "/compute/v1/join":
			var req compute.JoinRequest
			json.Unmarshal(body, &req)
			if len(req.Attempts) == 0 {
				json.NewEncoder(w).Encode(compute.JoinSalts{Salts: []string{"0123456789abcdef0123456789abcdef", fakeSalt}})
				return
			}
			key := compute.JoinKey("ABCD-EFGH", fakeSalt)
			var attempt *compute.JoinAttempt
			for i, a := range req.Attempts {
				if a.Salt == fakeSalt && a.Proof == compute.JoinProof(key, req.Nonce) {
					attempt = &req.Attempts[i]
				}
			}
			if attempt == nil {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"error": "that pairing code is not valid any more"})
				return
			}
			d, err := compute.OpenCredential(key, req.Nonce, attempt.Credential)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.digest = d
			signer := fogSigner
			if f.signer != nil {
				signer = *f.signer
			}
			ans := compute.JoinAnswer{Node: "n1", FogKey: signer.PublicKeyB64()}
			ans.Confirm = compute.JoinConfirm(key, req.Nonce, attempt.Credential, ans)
			json.NewEncoder(w).Encode(ans)
		case "/compute/v1/poll":
			if f.raw != nil {
				w.WriteHeader(f.raw.status)
				io.WriteString(w, f.raw.body)
				return
			}
			macFor := func(d string) string {
				return compute.RequestMAC(d, r.Header.Get(compute.HeaderNodeID), r.Header.Get(compute.HeaderNodeNonce),
					r.Header.Get(compute.HeaderNodeTime), r.Header.Get("Content-Type"), r.Header.Get(compute.HeaderBodySHA256))
			}
			used := digest
			if r.Header.Get(compute.HeaderNodeAuth) != macFor(digest) {
				used = ""
				if f.prev != "" && r.Header.Get(compute.HeaderNodeAuth) == macFor(f.prev) {
					used = f.prev
				}
			}
			if used == "" || r.Header.Get(compute.HeaderNodeID) != "n1" || compute.BodySHA256(body) != r.Header.Get(compute.HeaderBodySHA256) {
				f.sign(w, r, http.StatusForbidden, map[string]string{"error": "bad MAC", "code": compute.CodeBadRequest})
				return
			}
			if f.status != 0 {
				code := f.code
				if code == "" && f.status == http.StatusUnauthorized {
					code = compute.CodeUnknownMachine
				}
				f.sign(w, r, f.status, map[string]string{"error": "refused", "code": code})
				return
			}
			var rep compute.NodeReport
			json.Unmarshal(body, &rep)
			f.reports = append(f.reports, rep)
			if rep.Rotate != nil {
				raw, _ := base64.StdEncoding.DecodeString(rep.Rotate.Pub)
				mp, _ := ecdh.X25519().NewPublicKey(raw)
				priv, _ := ecdh.X25519().GenerateKey(nil)
				shared, _ := priv.ECDH(mp)
				fp := base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
				f.prev, f.digest = used, compute.RotatedDigest(shared, "n1", used, rep.Rotate.Pub, fp)
				f.rotations++
				f.sign(w, r, http.StatusOK, compute.NodeAnswer{Rotated: &compute.RotateAnswer{Pub: fp}})
				return
			}
			if rep.Wait && f.hold > 0 {
				f.mu.Unlock()
				select {
				case <-time.After(f.hold):
				case <-r.Context().Done():
				}
				f.mu.Lock()
			}
			f.sign(w, r, http.StatusOK, f.answer)
		case "/compute/v1/fog-node/" + runtime.GOOS + "-" + runtime.GOARCH:
			f.fetched++
			w.Write(f.binary)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestJoinKeepsThePairing(t *testing.T) {
	f := &fakeFog{}
	srv := f.server(t)
	defer srv.Close()
	dir := t.TempDir()
	if _, err := Join(context.Background(), dir, srv.URL+"/", "WRONG", "Mini", "/nonexistent/ffmpeg", "v1"); err == nil ||
		!strings.Contains(err.Error(), "not valid any more") {
		t.Fatalf("bad code: %v", err)
	}
	cfg, err := Join(context.Background(), dir, srv.URL+"/", "ABCD-EFGH", "Mini", "/nonexistent/ffmpeg", "v1")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(dir)
	if err != nil || loaded.Token != cfg.Token || loaded.Fog != srv.URL || loaded.Name != "Mini" || cfg.Node != "n1" {
		t.Fatalf("config: %+v %v", loaded, err)
	}
	info, _ := os.Stat(filepath.Join(dir, "config.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config readable by others: %v", info.Mode())
	}
}

func TestAgentSendsInventoryOnlyWhenItChanges(t *testing.T) {
	f := &fakeFog{answer: compute.NodeAnswer{PollMS: 250}}
	srv := f.server(t)
	defer srv.Close()
	cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
	a := &Agent{Dir: t.TempDir(), Config: cfg, Version: "v1", Log: quiet(), Pressed: func() bool { return false }}
	for range 3 {
		if err := a.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.reports[0].Inventory == nil || f.reports[0].Inventory.Name != "Mini" || !f.reports[0].Wait {
		t.Fatalf("first report: %+v", f.reports[0])
	}
	if f.reports[1].Inventory != nil || f.reports[2].Inventory != nil {
		t.Fatal("inventory sent again unchanged")
	}
	cfg.Busy = true
	a.Once(context.Background())
	if inv := f.reports[3].Inventory; inv == nil || !inv.Busy {
		t.Fatalf("busy not reported: %+v", inv)
	}
}

func TestAgentForgetsWhenRemoved(t *testing.T) {
	f := &fakeFog{status: http.StatusUnauthorized}
	srv := f.server(t)
	defer srv.Close()
	dir := t.TempDir()
	cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini"}
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Dir: dir, Config: cfg, Version: "v1", Log: quiet()}
	if err := a.Run(context.Background()); err != ErrUnpaired {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err != ErrNotPaired {
		t.Fatalf("pairing kept: %v", err)
	}
}

// stoppingWork is Work that notes being told to stop everything.
type stoppingWork struct {
	mu      sync.Mutex
	stopped bool
	changed chan struct{}
}

func (w *stoppingWork) Report() ([]string, []compute.Exited, map[string]float64) {
	return []string{"r1"}, nil, nil
}
func (w *stoppingWork) Start(compute.NodeRun) {}
func (w *stoppingWork) Stop(string)           {}
func (w *stoppingWork) Changed() <-chan struct{} {
	return w.changed
}
func (w *stoppingWork) StopAll(context.Context) {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

// fog answering a held poll with Removed: the machine stops every run at
// once, forgets the pairing, and says it's unpaired, so fog-node waits for
// a new one rather than failing over and over.
func TestAgentStopsItsRunsWhenRemoved(t *testing.T) {
	f := &fakeFog{answer: compute.NodeAnswer{Removed: true}}
	srv := f.server(t)
	defer srv.Close()
	dir := t.TempDir()
	cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
	cfg.Save(dir)
	work := &stoppingWork{changed: make(chan struct{})}
	a := &Agent{Dir: dir, Config: cfg, Version: "v1", Log: quiet(), Work: work, Pressed: func() bool { return false }}
	if err := a.Run(context.Background()); err != ErrUnpaired {
		t.Fatalf("run: %v", err)
	}
	if !work.stopped {
		t.Fatal("its runs weren't stopped")
	}
	if _, err := LoadConfig(dir); err != ErrNotPaired {
		t.Fatalf("pairing kept: %v", err)
	}
}

// releaseKey stands in for fog's release key: the one this build pins, for
// the length of a test.
func releaseKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	was := selfupdate.ReleaseKeys
	selfupdate.ReleaseKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	t.Cleanup(func() { selfupdate.ReleaseKeys = was })
	return priv
}

// release is a signed latest.json for version, naming this platform's
// fog-node by sum, issued now.
func release(key ed25519.PrivateKey, version, platformKey, sum string) ([]byte, string) {
	return releaseIssued(key, version, platformKey, sum, time.Now())
}

func releaseIssued(key ed25519.PrivateKey, version, platformKey, sum string, issued time.Time) ([]byte, string) {
	body, _ := json.Marshal(map[string]any{"version": version, "issued_at": issued.Unix(), "artifacts": map[string]any{
		platformKey: map[string]string{"url": platformKey, "sha256": sum}}})
	return body, base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))
}

func thisPlatform() string { return "fog-node-" + runtime.GOOS + "-" + runtime.GOARCH }

func update(key ed25519.PrivateKey, version, sum string) *compute.Update {
	m, sig := release(key, version, thisPlatform(), sum)
	return &compute.Update{Version: version, URL: "/compute/v1/fog-node/" + runtime.GOOS + "-" + runtime.GOARCH,
		SHA256: sum, Manifest: m, ManifestSig: sig}
}

func TestAgentUpdatesItself(t *testing.T) {
	key := releaseKey(t)
	body := []byte("new fog-node")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	f := &fakeFog{binary: body}
	srv := f.server(t)
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "fog-node")
	os.WriteFile(exe, []byte("old"), 0o755)
	exited := -1
	cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
	a := &Agent{Dir: t.TempDir(), Config: cfg, Version: "v1.0.0", Log: quiet(), Exe: exe, Exit: func(c int) { exited = c }}

	// A build that isn't the one the signed release names leaves the
	// binary alone, tells fog why with the next inventory, and isn't tried
	// again for that version.
	f.answer = compute.NodeAnswer{Update: update(key, "v2.0.0", strings.Repeat("0", 64))}
	a.Once(context.Background())
	if got, _ := os.ReadFile(exe); string(got) != "old" || exited != -1 {
		t.Fatalf("bad download installed: %q, exit %d", got, exited)
	}
	f.answer = compute.NodeAnswer{}
	a.Once(context.Background())
	inv := f.reports[len(f.reports)-1].Inventory
	if inv == nil || inv.UpdateFailed == nil || inv.UpdateFailed.Version != "v2.0.0" || !strings.Contains(inv.UpdateFailed.Error, "checksum mismatch") {
		t.Fatalf("failure not reported: %+v", inv)
	}
	// Retried only when fog says so: Update now.
	u := update(key, "v2.0.0", good)
	f.answer = compute.NodeAnswer{Update: u}
	a.Once(context.Background())
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatalf("retried a failed version unasked: %q", got)
	}
	u.Retry = true
	a.Once(context.Background())
	if got, _ := os.ReadFile(exe); string(got) != "new fog-node" || exited != 0 {
		t.Fatalf("retry: %q, exit %d", got, exited)
	}
	os.WriteFile(exe, []byte("old"), 0o755)
	exited = -1

	f.answer.Update = update(key, "v3.0.0", good)
	a.Once(context.Background())
	if got, _ := os.ReadFile(exe); string(got) != "new fog-node" || exited != 0 {
		t.Fatalf("update: %q, exit %d", got, exited)
	}
	if f.sawToken {
		t.Fatal("the machine's credential was sent")
	}
}

// An update is installed only as fog's signed release: not one whose
// manifest another key signed, nor one the manifest doesn't cover, nor one
// for another version, nor one fetched from anywhere but fog.
func TestAnUpdateMustBeFogsSignedRelease(t *testing.T) {
	key := releaseKey(t)
	_, otherKey, _ := ed25519.GenerateKey(nil)
	body := []byte("evil fog-node")
	sum := sha256.Sum256(body)
	evil := hex.EncodeToString(sum[:])
	var elsewhere atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Write(body)
	}))
	defer attacker.Close()

	unsigned := update(key, "v2.0.0", evil)
	unsigned.Manifest, unsigned.ManifestSig = nil, ""
	notCovered := update(key, "v2.0.0", evil)
	notCovered.Manifest, notCovered.ManifestSig = release(key, "v2.0.0", "fog-node-plan9-mips", evil)
	otherVersion := update(key, "v2.0.0", evil)
	otherVersion.Version = "v9.0.0"
	fromElsewhere := update(key, "v2.0.0", evil)
	fromElsewhere.URL = attacker.URL + "/fog-node"
	// An old release, validly signed, replayed: older than selfupdate's
	// MaxManifestAge, so refused like everywhere else.
	stale := update(key, "v2.0.0", evil)
	stale.Manifest, stale.ManifestSig = releaseIssued(key, "v2.0.0", thisPlatform(), evil, time.Now().Add(-selfupdate.MaxManifestAge-24*time.Hour))
	tampered := update(key, "v2.0.0", strings.Repeat("1", 64))
	tampered.Manifest = []byte(strings.Replace(string(tampered.Manifest), strings.Repeat("1", 64), evil, 1))
	for name, u := range map[string]*compute.Update{
		"signed by another key": update(otherKey, "v2.0.0", evil),
		"no manifest":           unsigned,
		"not covered":           notCovered,
		"another version":       otherVersion,
		"from another host":     fromElsewhere,
		"manifest tampered":     tampered,
		"older than 180 days":   stale,
	} {
		f := &fakeFog{binary: body}
		srv := f.server(t)
		exe := filepath.Join(t.TempDir(), "fog-node")
		os.WriteFile(exe, []byte("old"), 0o755)
		cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
		exited := -1
		a := &Agent{Dir: t.TempDir(), Config: cfg, Version: "v1.0.0", Log: quiet(), Exe: exe, Exit: func(c int) { exited = c }}
		u.Retry = true
		f.answer = compute.NodeAnswer{Update: u}
		a.Once(context.Background())
		if got, _ := os.ReadFile(exe); string(got) != "old" || exited != -1 || a.updateFailed == nil {
			t.Errorf("%s: installed %q (exit %d, failed %+v)", name, got, exited, a.updateFailed)
		}
		if f.sawToken {
			t.Errorf("%s: the machine's credential was sent", name)
		}
		srv.Close()
	}
	if elsewhere.Load() != 0 {
		t.Error("a build was fetched from another host")
	}
}

// fog-node only moves forward: a genuine signed release older than the one
// it runs is refused (and fog told why), the same one is skipped quietly.
func TestAnUpdateMustBeNewer(t *testing.T) {
	key := releaseKey(t)
	body := []byte("old fog-node")
	sum := sha256.Sum256(body)
	f := &fakeFog{binary: body}
	srv := f.server(t)
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "fog-node")
	os.WriteFile(exe, []byte("current"), 0o755)
	cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
	exited := -1
	a := &Agent{Dir: t.TempDir(), Config: cfg, Version: "v1.4.2", Log: quiet(), Exe: exe, Exit: func(c int) { exited = c }}
	for _, older := range []string{"v1.4.1", "v1.3.9", "v1.4.2-rc.1", "v0.9.0"} {
		u := update(key, older, hex.EncodeToString(sum[:]))
		u.Retry = true
		f.answer = compute.NodeAnswer{Update: u}
		a.updateFailed = nil
		a.Once(context.Background())
		if got, _ := os.ReadFile(exe); string(got) != "current" || exited != -1 || f.fetched != 0 {
			t.Fatalf("%s: rolled back (%q, exit %d, fetched %d)", older, got, exited, f.fetched)
		}
		if a.updateFailed == nil || a.updateFailed.Version != older || !strings.Contains(a.updateFailed.Error, "isn't newer") {
			t.Fatalf("%s: fog isn't told why: %+v", older, a.updateFailed)
		}
	}
	a.updateFailed = nil
	f.answer = compute.NodeAnswer{Update: update(key, "v1.4.2", hex.EncodeToString(sum[:]))}
	a.Once(context.Background())
	if got, _ := os.ReadFile(exe); string(got) != "current" || f.fetched != 0 || a.updateFailed != nil {
		t.Fatalf("the same version wasn't skipped quietly: %q fetched %d %+v", got, f.fetched, a.updateFailed)
	}
}

// A man in the middle's answers, whatever their status, are never read:
// not an unsigned 202 that starts runs, nor a 204, nor a redirect. And of
// fog's own signed answers, only "no such machine" unpairs: a signed
// refusal for a bad MAC, a replay or a clock that's off (which someone
// between them could provoke) is a failed poll, tried again.
func TestOnlyASignedAnswerIsRead(t *testing.T) {
	attack := `{"start":[{"id":"evil"}],"stop":["r0"],"removed":true}`
	for name, f := range map[string]*fakeFog{
		"unsigned 202":            {raw: &rawAnswer{http.StatusAccepted, attack}},
		"unsigned 204":            {raw: &rawAnswer{http.StatusNoContent, ""}},
		"unsigned 302":            {raw: &rawAnswer{http.StatusFound, attack}},
		"unsigned 401":            {raw: &rawAnswer{http.StatusUnauthorized, `{"code":"unknown_machine"}`}},
		"unsigned 500":            {raw: &rawAnswer{http.StatusInternalServerError, attack}},
		"signed 202":              {status: http.StatusAccepted},
		"signed 403, bad MAC":     {status: http.StatusForbidden, code: compute.CodeBadRequest},
		"signed 403, replayed":    {status: http.StatusForbidden, code: compute.CodeReplay},
		"signed 403, clock":       {status: http.StatusForbidden, code: compute.CodeClock},
		"signed 401, not unknown": {status: http.StatusUnauthorized, code: compute.CodeReplay},
		"signed 500":              {status: http.StatusInternalServerError},
	} {
		srv := f.server(t)
		dir := t.TempDir()
		cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
		cfg.Save(dir)
		work := &recordingWork{changed: make(chan struct{})}
		a := &Agent{Dir: dir, Config: cfg, Version: "v1.0.0", Log: quiet(), Work: work, Pressed: func() bool { return false }}
		err := a.Once(context.Background())
		if err == nil || errors.Is(err, ErrUnpaired) {
			t.Errorf("%s: %v", name, err)
		}
		if name == "signed 403, clock" && !errors.Is(err, errClock) {
			t.Errorf("clock: %v", err)
		}
		if len(work.started) != 0 || len(work.stopped) != 0 || work.stoppedAll {
			t.Errorf("%s: acted on it: %+v", name, work)
		}
		if _, err := LoadConfig(dir); err != nil {
			t.Errorf("%s: forgot the pairing: %v", name, err)
		}
		srv.Close()
	}
	// Run as the service: the refusal is retried, and the pairing kept.
	f := &fakeFog{status: http.StatusForbidden, code: compute.CodeClock}
	srv := f.server(t)
	defer srv.Close()
	dir := t.TempDir()
	cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
	cfg.Save(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	a := &Agent{Dir: dir, Config: cfg, Version: "v1.0.0", Log: quiet(), Pressed: func() bool { return false }}
	if err := a.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run: %v", err)
	}
	if _, err := LoadConfig(dir); err != nil {
		t.Fatalf("forgot the pairing: %v", err)
	}
}

// Someone answering in fog's place, unsigned or with a key of its own,
// gets nothing done: no run started or stopped, no update, no removal, and
// never the machine's credential.
func TestAgentIgnoresAnswersFogDidntSign(t *testing.T) {
	key := releaseKey(t)
	body := []byte("evil fog-node")
	sum := sha256.Sum256(body)
	impostor := compute.NewKeySigner()
	for name, f := range map[string]*fakeFog{
		"unsigned":        {unsigned: true},
		"another key":     {signer: &impostor},
		"another key 401": {signer: &impostor, status: http.StatusUnauthorized},
		"unsigned 401":    {unsigned: true, status: http.StatusUnauthorized},
	} {
		f.binary = body
		f.answer = compute.NodeAnswer{Start: []compute.NodeRun{{ID: "r1"}}, Stop: []string{"r0"}, Removed: true,
			Update: update(key, "v2.0.0", hex.EncodeToString(sum[:]))}
		srv := f.server(t)
		dir := t.TempDir()
		cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"}
		cfg.Save(dir)
		exe := filepath.Join(t.TempDir(), "fog-node")
		os.WriteFile(exe, []byte("old"), 0o755)
		work := &recordingWork{changed: make(chan struct{})}
		a := &Agent{Dir: dir, Config: cfg, Version: "v1.0.0", Log: quiet(), Work: work, Exe: exe, Exit: func(int) {}, Pressed: func() bool { return false }}
		err := a.Once(context.Background())
		if !errors.Is(err, errUnsigned) {
			t.Errorf("%s: %v", name, err)
		}
		if len(work.started) != 0 || len(work.stopped) != 0 || work.stoppedAll {
			t.Errorf("%s: acted on it: %+v", name, work)
		}
		if got, _ := os.ReadFile(exe); string(got) != "old" || f.fetched != 0 {
			t.Errorf("%s: updated", name)
		}
		if _, err := LoadConfig(dir); err != nil {
			t.Errorf("%s: forgot the pairing: %v", name, err)
		}
		if f.sawToken {
			t.Errorf("%s: the machine's credential was sent", name)
		}
		srv.Close()
	}
}

// A pairing pins fog's key, and only fog's answer can carry it: one from
// someone who doesn't hold the code is refused, and nothing is kept.
func TestJoinPinsFogsKey(t *testing.T) {
	f := &fakeFog{}
	srv := f.server(t)
	defer srv.Close()
	dir := t.TempDir()
	cfg, err := Join(context.Background(), dir, srv.URL, "abcd efgh", "Mini", "/nonexistent/ffmpeg", "v1")
	if err != nil || cfg.FogKey != fogKey || !strings.HasPrefix(cfg.Token, compute.TokenPrefix) {
		t.Fatalf("join: %+v %v", cfg, err)
	}
	if f.sawToken || f.sawDigest || f.digest != compute.TokenDigest(cfg.Token) {
		t.Fatal("the credential, or its digest, went out in the clear")
	}
	// Its first poll rotates the credential, and it polls as the new one,
	// never sending either: the pairing's digest is retired.
	paired := f.digest
	a := &Agent{Dir: dir, Config: cfg, Version: "v1", Log: quiet(), Pressed: func() bool { return false }}
	if err := a.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.rotations != 1 || f.digest == paired || cfg.Secret() != f.digest || cfg.Token != "" || !cfg.Rotated {
		t.Fatalf("rotation: %d %+v", f.rotations, cfg)
	}
	saved, _ := LoadConfig(dir)
	if saved.Secret() != f.digest || saved.Token != "" {
		t.Fatalf("not kept: %+v", saved)
	}
	if err := a.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.rotations != 1 || f.prev != paired {
		t.Fatal("rotated again")
	}
	if f.sawToken || f.sawDigest {
		t.Fatal("a poll carried the credential or its digest")
	}
	mitm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ans := compute.JoinAnswer{Node: "n1", FogKey: compute.NewKeySigner().PublicKeyB64(), Confirm: "forged"}
		json.NewEncoder(w).Encode(ans)
	}))
	defer mitm.Close()
	dir2 := t.TempDir()
	if _, err := Join(context.Background(), dir2, mitm.URL, "ABCD-EFGH", "Mini", "/nonexistent/ffmpeg", "v1"); err == nil {
		t.Fatal("paired with someone who doesn't hold the code")
	}
	if _, err := LoadConfig(dir2); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("kept a pairing: %v", err)
	}
	// A pairing from before fog-node pinned fog's key lends nothing.
	(&Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", Name: "Mini"}).Save(dir2)
	if _, err := LoadConfig(dir2); !errors.Is(err, ErrNotPaired) || !errors.Is(err, ErrRepair) {
		t.Fatalf("an old pairing: %v", err)
	}
}

// recordingWork notes what it's told to do.
type recordingWork struct {
	started    []string
	stopped    []string
	stoppedAll bool
	changed    chan struct{}
}

func (w *recordingWork) Report() ([]string, []compute.Exited, map[string]float64) {
	return []string{}, nil, nil
}
func (w *recordingWork) Start(r compute.NodeRun)  { w.started = append(w.started, r.ID) }
func (w *recordingWork) Stop(id string)           { w.stopped = append(w.stopped, id) }
func (w *recordingWork) Changed() <-chan struct{} { return w.changed }
func (w *recordingWork) StopAll(context.Context)  { w.stoppedAll = true }

func TestServiceDefinitionRunsThisBinary(t *testing.T) {
	s := Service{Exe: "/Users/me/Library/Application Support/fog-node/bin/fog-node", Dir: "/Users/me/Library/Application Support/fog-node"}
	def := s.Definition()
	if !strings.Contains(def, "fog-node/bin/fog-node") || !strings.Contains(def, "run") || !strings.Contains(def, "--dir") {
		t.Fatalf("definition:\n%s", def)
	}
}

// A poll macOS refuses for Local Network privacy ends the process for
// launchd to start a fresh one, which has the access once it's given;
// restarts slow down while it stays refused, and the poll that gets
// through says so.
func TestAgentRestartsForLocalNetworkAccess(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Local Network privacy is macOS's")
	}
	// A listener that's closed: then a fake fog that refuses the route.
	var refused atomic.Bool
	refused.Store(true)
	f := &fakeFog{}
	srv := f.server(t)
	defer srv.Close()
	dir := t.TempDir()
	cfg := &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Rotated: true, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg", InferenceOff: true}
	logs := &lockedLog{}
	var waited []time.Duration
	exited := -1
	a := &Agent{Dir: dir, Config: cfg, Version: "v1", Log: slog.New(slog.NewTextHandler(logs, nil)), Pressed: func() bool { return false },
		Exit: func(c int) { exited = c },
		wait: func(d time.Duration) <-chan time.Time {
			waited = append(waited, d)
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		}}
	a.Client = &Client{Fog: srv.URL, Token: "fognode_x", Node: "n1", FogKey: fogKey, HTTP: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if refused.Load() {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}}

	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exited != exitLocalNetwork || len(waited) != 1 || waited[0] != localNetworkQuick {
		t.Fatalf("exit %d after waiting %v", exited, waited)
	}
	if !strings.Contains(logs.String(), "Privacy & Security › Local Network") {
		t.Fatalf("log: %s", logs.String())
	}

	// Still refused long after the first time: restarts every half minute.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, localNetworkFile), old, old)
	exited = -1
	a.Run(context.Background())
	if waited[1] != localNetworkPatient || exited != exitLocalNetwork {
		t.Fatalf("patient restart: waited %v, exit %d", waited, exited)
	}

	// Allowed: the next process gets through, says so, and forgets.
	refused.Store(false)
	logs.reset()
	ctx, cancel := context.WithCancel(context.Background())
	f.answer = compute.NodeAnswer{PollMS: 250}
	go func() {
		for !strings.Contains(logs.String(), "allows fog-node on the local network now") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	a.Run(ctx)
	if _, err := os.Stat(filepath.Join(dir, localNetworkFile)); !os.IsNotExist(err) {
		t.Fatal("the refusal wasn't forgotten")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// lockedLog is a log buffer a test can read while the agent writes it.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}
