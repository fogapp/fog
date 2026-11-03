package computenode

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
	"gitea.lan/matthew/fog/internal/selfupdate"
)

// Client talks to fogd's /compute/v1. With FogKey, it acts on an answer
// only when fog signed it, for Node and the nonce this request carried;
// anything else is errUnsigned, whoever sent it.
type Client struct {
	Fog   string
	Token string
	// Digest is the credential's digest, when the machine holds that and
	// not a token (it rotated); "" derives it from Token.
	Digest string
	Node   string
	FogKey string
	HTTP   *http.Client
}

func (c *Client) digest() string {
	if c.Digest != "" {
		return c.Digest
	}
	return compute.TokenDigest(c.Token)
}

// errUnsigned is an answer fog didn't sign: someone else on the network
// answering in fog's place. It's ignored, like a poll that failed.
var errUnsigned = errors.New("an answer not signed by the fog this machine is paired with; ignoring it")

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: compute.HoldPoll + 20*time.Second}
}

// post sends JSON and decodes the answer; a refusal comes back with fog's
// own words when it sent {errors: [...]}.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.Fog, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var pub ed25519.PublicKey
	nonce := ""
	if c.FogKey != "" {
		// Paired: the credential is proven, never sent, and only fog's
		// signed answer counts.
		if pub, err = compute.ParseFogKey(c.FogKey); err != nil {
			return err
		}
		nonce = compute.NewNonce()
		at := time.Now().UTC().Format(time.RFC3339Nano)
		sum := compute.BodySHA256(raw)
		req.Header.Set(compute.HeaderNodeNonce, nonce)
		req.Header.Set(compute.HeaderNodeID, c.Node)
		req.Header.Set(compute.HeaderNodeTime, at)
		req.Header.Set(compute.HeaderBodySHA256, sum)
		req.Header.Set(compute.HeaderNodeAuth, compute.RequestMAC(c.digest(), c.Node, nonce, at, "application/json", sum))
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if pub != nil {
		// Paired: nothing fog didn't sign is read at all, whatever its
		// status. Of what it did sign, only a 200 is acted on, and only a
		// 401 saying fog has no such machine unpairs this one; any other
		// answer is a failed poll, tried again.
		if !compute.VerifyAnswer(pub, c.Node, nonce, data, resp.Header.Get(compute.HeaderFogSignature)) {
			return errUnsigned
		}
		var refusal struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		switch {
		case resp.StatusCode == http.StatusOK:
		case resp.StatusCode == http.StatusUnauthorized && json.Unmarshal(data, &refusal) == nil && refusal.Code == compute.CodeUnknownMachine:
			return ErrUnpaired
		case json.Unmarshal(data, &refusal) == nil && refusal.Code == compute.CodeClock:
			return fmt.Errorf("%w: %s", errClock, refusal.Error)
		default:
			json.Unmarshal(data, &refusal)
			return fmt.Errorf("fog refused the poll (%s, %s): %s", resp.Status, refusal.Code, refusal.Error)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(data, out)
	}
	if resp.StatusCode/100 != 2 {
		var refusal struct {
			Error  string   `json:"error"`
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(data, &refusal) == nil {
			if refusal.Error != "" {
				return errors.New(refusal.Error)
			}
			if len(refusal.Errors) > 0 {
				return errors.New(strings.Join(refusal.Errors, "; "))
			}
		}
		return fmt.Errorf("fog answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ErrUnpaired is fog saying, signed, that it has no such machine.
var ErrUnpaired = errors.New("fog no longer knows this machine")

// errClock is fog refusing a poll because this machine's clock is off.
var errClock = errors.New("this machine's clock is too far from fog's: set its time right (NTP)")

// Join pairs this machine with fog using a code from Settings › Compute,
// and keeps the pairing in dir. The code is never sent: the machine proves
// it holds it, fog proves the same over its answer, and the machine pins
// the key fog's answer names. Its credential it makes itself, and fog gets
// only the digest, sealed under the code; its first poll then rotates it
// (internal/compute/trust.go).
func Join(ctx context.Context, dir, fog, code, name, ffmpeg, version string) (*Config, error) {
	fog = strings.TrimRight(strings.TrimSpace(fog), "/")
	inv := Probe(ctx, name, FindFFmpeg(ffmpeg), version)
	c := &Client{Fog: fog}
	// Each live code has its own salt, and the machine can't tell which is
	// its code's: it pairs under each.
	var salts compute.JoinSalts
	if err := c.post(ctx, "/compute/v1/join", compute.JoinRequest{}, &salts); err != nil {
		return nil, err
	}
	if len(salts.Salts) == 0 {
		return nil, errors.New("fog has no live pairing code: make one in Settings › Compute")
	}
	if len(salts.Salts) > compute.MaxJoinAttempts {
		salts.Salts = salts.Salts[:compute.MaxJoinAttempts]
	}
	nonce := compute.NewNonce()
	token := compute.TokenPrefix + compute.NewNonce()
	req := compute.JoinRequest{Nonce: nonce, Inventory: inv}
	keys := make([][]byte, len(salts.Salts))
	for i, salt := range salts.Salts {
		keys[i] = compute.JoinKey(code, salt)
		req.Attempts = append(req.Attempts, compute.JoinAttempt{Salt: salt, Proof: compute.JoinProof(keys[i], nonce),
			Credential: compute.SealCredential(keys[i], nonce, compute.TokenDigest(token))})
	}
	var ans compute.JoinAnswer
	if err := c.post(ctx, "/compute/v1/join", req, &ans); err != nil {
		return nil, err
	}
	var checked error = errors.New("no pairing attempt was answered")
	for i, a := range req.Attempts {
		if _, checked = compute.CheckJoinAnswer(keys[i], nonce, a.Credential, ans); checked == nil {
			break
		}
	}
	if checked != nil {
		return nil, checked
	}
	cfg := &Config{Fog: fog, Node: ans.Node, Token: token, FogKey: ans.FogKey, Name: inv.Name, FFmpeg: ffmpeg}
	return cfg, cfg.Save(dir)
}

// Work is what runs fog's runs on this machine: the Runner. Nil runs none.
type Work interface {
	// Report is what's running, what ended since the last call (each said
	// once), and how far along.
	Report() (running []string, exited []compute.Exited, progress map[string]float64)
	Start(run compute.NodeRun)
	Stop(id string)
	// Changed is signalled when a run ends, so the agent reports it now
	// rather than at the end of a held poll.
	Changed() <-chan struct{}
}

// Lending is the model server the machine lends: the Lender.
type Lending interface {
	// Inventory is what it lends now; nil for nothing.
	Inventory() *compute.InferenceCaps
	// Changed is signalled when that changes, so fog hears of it now.
	Changed() <-chan struct{}
}

// videoReporter is Work that says how it runs video: its confinement and
// limits (the Runner).
type videoReporter interface {
	Video(ctx context.Context, v *compute.VideoCaps)
}

// stopper is Work that can end everything it runs (the Runner).
type stopper interface {
	StopAll(ctx context.Context)
}

// Agent is the node's loop: poll fog, report, take what it hands over.
type Agent struct {
	Dir     string
	Config  *Config
	Version string
	Log     *slog.Logger
	Work    Work
	// Lending is the model server lent to fog; nil lends none.
	Lending Lending
	// Client overrides the one made from Config (tests).
	Client *Client
	// Exe is this binary, replaced by an update; empty disables updates.
	Exe string
	// Reprobe is how often the machine is asked again what it can do.
	Reprobe time.Duration
	// Exit ends the process once a new binary is in place, for launchd or
	// systemd to start it.
	Exit func(code int)
	// Pressed reports the machine short of memory; nil asks the kernel.
	Pressed func() bool
	// wait stands in for time.After (tests).
	wait func(time.Duration) <-chan time.Time

	inventory compute.Inventory
	probedAt  time.Time
	sentInv   string
	// updateFailed is the update this process couldn't put in place, told
	// to fog with the inventory; it isn't tried again unless fog asks.
	updateFailed *compute.UpdateFailed
}

func (a *Agent) client() *Client {
	if a.Client != nil {
		return a.Client
	}
	return &Client{Fog: a.Config.Fog, Digest: a.Config.Secret(), Node: a.Config.Node, FogKey: a.Config.FogKey}
}

// Run polls until ctx ends or fog removes the machine. Removed, it stops
// every run, forgets the pairing, and answers ErrUnpaired.
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := a.Once(ctx)
		switch {
		case errors.Is(err, errRemoved), errors.Is(err, ErrUnpaired):
			a.Log.Warn("fog removed this machine: stopping its runs and forgetting the pairing")
			if s, ok := a.Work.(stopper); ok {
				stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				s.StopAll(stopCtx)
				cancel()
			}
			if err := Forget(a.Dir); err != nil {
				return err
			}
			return ErrUnpaired
		case err != nil && localNetworkRefused(err) && ctx.Err() == nil:
			if a.awaitLocalNetwork(ctx) {
				// Exit for launchd to start a process that has the access.
				a.Exit(exitLocalNetwork)
				return nil
			}
			fallthrough
		case err != nil && ctx.Err() == nil:
			a.Log.Warn("poll failed", "err", err, "retry", backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		default:
			backoff = time.Second
			a.localNetworkAllowed()
		}
	}
	return ctx.Err()
}

// exitLocalNetwork is fog-node's exit status when it ends itself for macOS
// to grant its next process Local Network access (EX_TEMPFAIL).
const exitLocalNetwork = 75

var errRemoved = errors.New("removed")

// Once is one poll: the report, a held wait for work, and what came back.
func (a *Agent) Once(ctx context.Context) error {
	report := compute.NodeReport{Running: []string{}, Wait: true}
	if inv := a.currentInventory(ctx); inv != nil {
		report.Inventory = inv
	}
	// The first poll after pairing rotates the credential: an X25519
	// exchange whose secret only this poll and fog's answer share.
	var rotate *ecdh.PrivateKey
	if !a.Config.Rotated && a.Config.FogKey != "" {
		var err error
		if rotate, err = ecdh.X25519().GenerateKey(rand.Reader); err != nil {
			return err
		}
		report.Rotate = &compute.RotateRequest{Pub: base64.StdEncoding.EncodeToString(rotate.PublicKey().Bytes())}
		report.Wait = false
	}
	var changed, lent <-chan struct{}
	if a.Work != nil {
		report.Running, report.Exited, report.Progress = a.Work.Report()
		if len(report.Exited) > 0 {
			report.Wait = false
		}
		changed = a.Work.Changed()
	}
	if a.Lending != nil {
		lent = a.Lending.Changed()
	}
	// A run ending, or the model server lent changing, while the poll is
	// held cuts the hold short, so fog hears of it now.
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-changed:
			cancel()
		case <-lent:
			cancel()
		case <-pollCtx.Done():
		}
	}()
	var ans compute.NodeAnswer
	err := a.client().post(pollCtx, "/compute/v1/poll", report, &ans)
	if err != nil && pollCtx.Err() != nil && ctx.Err() == nil {
		// Cut short for a report; what ended goes out on the next poll.
		a.requeue(report)
		return nil
	}
	if err != nil {
		a.requeue(report)
		return err
	}
	if report.Inventory != nil {
		raw, _ := json.Marshal(report.Inventory)
		a.sentInv = string(raw)
	}
	if ans.Removed {
		return errRemoved
	}
	if rotate != nil && ans.Rotated != nil {
		if err := a.rotated(rotate, report.Rotate.Pub, *ans.Rotated); err != nil {
			return err
		}
	}
	if a.Work != nil {
		for _, id := range ans.Stop {
			a.Work.Stop(id)
		}
		for _, run := range ans.Start {
			a.Work.Start(run)
		}
	}
	if ans.Update != nil {
		a.update(ctx, *ans.Update)
	}
	return nil
}

// requeue hands back exits a failed poll didn't deliver.
func (a *Agent) requeue(report compute.NodeReport) {
	if r, ok := a.Work.(interface{ Requeue([]compute.Exited) }); ok && len(report.Exited) > 0 {
		r.Requeue(report.Exited)
	}
}

// currentInventory answers the machine's inventory when fog hasn't heard
// it yet or it changed, probing again now and then.
func (a *Agent) currentInventory(ctx context.Context) *compute.Inventory {
	every := a.Reprobe
	if every == 0 {
		every = 10 * time.Minute
	}
	if a.probedAt.IsZero() || time.Since(a.probedAt) > every {
		a.inventory = Probe(ctx, a.Config.Name, FindFFmpeg(a.Config.FFmpeg), a.Version)
		if v, ok := a.Work.(videoReporter); ok {
			v.Video(ctx, a.inventory.Video)
		}
		a.probedAt = time.Now()
	}
	inv := a.inventory
	// What the listener lends now, exactly: the Lender keeps both.
	inv.Inference = nil
	if a.Lending != nil {
		inv.Inference = a.Lending.Inventory()
	}
	// Read each poll, so `fog-node busy on` takes effect at once; and a
	// machine short of memory takes no batch work either.
	if cfg, err := LoadConfig(a.Dir); err == nil {
		a.Config.Busy = cfg.Busy
	}
	pressed := a.Pressed
	if pressed == nil {
		pressed = memoryPressed
	}
	inv.Busy = a.Config.Busy || pressed()
	inv.UpdateFailed = a.updateFailed
	raw, _ := json.Marshal(inv)
	if string(raw) == a.sentInv {
		return nil
	}
	return &inv
}

// update replaces this binary with the one fog offers, checked against its
// digest, and exits for the service manager to start it. A failure is
// logged and told to fog, and the node carries on as it is; it tries that
// version again only when fog says to retry.
func (a *Agent) update(ctx context.Context, u compute.Update) {
	if u.Version == a.Version {
		return
	}
	// Only forward: fog (or someone posing as it with a genuine old,
	// signed release) can't walk this machine back onto an older one.
	if !selfupdate.IsNewer(u.Version, a.Version) {
		if f := a.updateFailed; f == nil || f.Version != u.Version {
			a.updateFailed = &compute.UpdateFailed{Version: u.Version,
				Error: "fog offered " + u.Version + ", which isn't newer than this machine's " + a.Version + "; fog-node only moves forward"}
			a.Log.Warn("refused an update that isn't newer", "offered", u.Version, "running", a.Version)
		}
		return
	}
	if f := a.updateFailed; f != nil && f.Version == u.Version && !u.Retry {
		return
	}
	err := errors.New("this fog-node has no binary of its own to replace (it wasn't started from one)")
	if a.Exe != "" {
		err = a.replace(ctx, u)
	}
	if err != nil {
		a.updateFailed = &compute.UpdateFailed{Version: u.Version, Error: err.Error()}
		a.Log.Warn("update failed", "version", u.Version, "err", err)
		return
	}
	a.Log.Info("updated; restarting", "from", a.Version, "to", u.Version)
	if a.Exit != nil {
		a.Exit(0)
	}
}

// replace installs the fog-node fog offers, only once it is proven fog's
// release: the manifest must verify against the release key this build
// pins, be the version offered, and name the digest of the bytes fetched.
// The build is fetched from fog's own address, the only host this
// machine's credential is ever sent to.
func (a *Agent) replace(ctx context.Context, u compute.Update) error {
	want, err := releasedDigest(u)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(u.URL, "/compute/v1/fog-node/") || strings.Contains(u.URL, "..") || strings.ContainsAny(u.URL, "?#\\") {
		return fmt.Errorf("fog offered fog-node from %q, not from itself", u.URL)
	}
	url := strings.TrimRight(a.Config.Fog, "/") + u.URL
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	// The build is public, and checked against the signed release: the
	// machine's credential goes nowhere with it.
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.Exe), ".fog-node-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 256<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch: got %s, the signed release says %s", got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.Exe)
}

// releasedDigest is the digest fog's signed release manifest gives this
// machine's fog-node build, once the manifest verifies against the release
// key this build pins and is the version offered.
func releasedDigest(u compute.Update) (string, error) {
	if len(u.Manifest) == 0 || u.ManifestSig == "" {
		return "", errors.New("fog offered an update with no signed release manifest")
	}
	if err := selfupdate.VerifyReleaseManifest(u.Manifest, []byte(u.ManifestSig)); err != nil {
		return "", fmt.Errorf("the update's release manifest: %w", err)
	}
	var m selfupdate.Manifest
	if err := json.Unmarshal(u.Manifest, &m); err != nil {
		return "", fmt.Errorf("the update's release manifest can't be read: %w", err)
	}
	if m.Version != u.Version {
		return "", fmt.Errorf("the signed release is %s, not the %s offered", m.Version, u.Version)
	}
	art, ok := m.Artifacts["fog-node-"+runtime.GOOS+"-"+runtime.GOARCH]
	if !ok || len(art.SHA256) != 64 {
		return "", fmt.Errorf("the signed release %s has no fog-node for %s-%s", m.Version, runtime.GOOS, runtime.GOARCH)
	}
	return art.SHA256, nil
}

// rotated takes fog's half of a rotation: the new credential is the
// digest of the secret the two halves share, kept from now on in place of
// the token.
func (a *Agent) rotated(priv *ecdh.PrivateKey, myPub string, ans compute.RotateAnswer) error {
	raw, err := base64.StdEncoding.DecodeString(ans.Pub)
	if err != nil {
		return errors.New("fog's half of the rotation isn't a key")
	}
	fogPub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return errors.New("fog's half of the rotation isn't a key")
	}
	shared, err := priv.ECDH(fogPub)
	if err != nil {
		return err
	}
	a.Config.rotated(compute.RotatedDigest(shared, a.Config.Node, a.client().digest(), myPub, ans.Pub))
	if err := a.Config.Save(a.Dir); err != nil {
		return err
	}
	a.Log.Info("rotated this machine's credential")
	return nil
}
