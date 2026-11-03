package signin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/backoff"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/store"
)

// Signer signs a logout token (internal/signing.Keyring).
type Signer interface {
	Sign(claims any, typ string) (string, error)
}

const (
	// stuckAfter is when an undelivered logout becomes the operator's
	// business: an Inbox notice saying which app hasn't heard.
	stuckAfter = 24 * time.Hour
	// giveUpAfter is when it stops mattering: no fog session outlives 30
	// days, and an app caps its own at the fog_session_exp its ID token
	// stated, so by now the app has ended it itself.
	giveUpAfter = 30 * 24 * time.Hour
)

// Deliverer sends what the outbox owes: back-channel logout tokens
// (docs/signin-contract.md), signed fresh for each attempt.
type Deliverer struct {
	Store *store.Store
	Keys  Signer
	HTTP  *http.Client
	Log   *slog.Logger
	Now   func() time.Time
	// Issuer is fog's issuer now ("" without a domain: nothing is sent).
	Issuer func() string
	// Instance is this household's instance ULID, the first half of a
	// person address.
	Instance func() (string, error)
	// Notice files an Inbox notice (kind, app, title, body).
	Notice func(kind, app, title, body string) error
	// Visit tells a paired fog that a session of ours it hosts as a
	// visitor ended (visit_logout rows, target "peer:<id>").
	Visit func(ctx context.Context, peerID int64, sid string) error

	// running serialises Run: the loop and a kick after a sign-out must not
	// both read the same due row and deliver it twice.
	running sync.Mutex
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Run delivers everything due, once. A ticker calls it; so does anything
// that just enqueued, to save the wait.
func (d *Deliverer) Run(ctx context.Context) {
	d.running.Lock()
	defer d.running.Unlock()
	iss := d.Issuer()
	if iss == "" {
		return
	}
	rows, err := d.Store.DueAppOutbox(d.now(), 100)
	if err != nil {
		d.Log.Error("outbox", "err", err)
		return
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		d.deliver(ctx, iss, r)
	}
}

func (d *Deliverer) deliver(ctx context.Context, iss string, r store.AppOutboxRow) {
	age := d.now().Sub(r.CreatedAt)
	if r.Kind == store.AppOutboxVisitLogout {
		d.deliverVisit(ctx, r, age)
		return
	}
	client, err := d.Store.OIDCClient(r.Target)
	if err != nil || client.BackchannelURI == "" || r.Kind != store.AppOutboxLogout || age > giveUpAfter {
		// The app is gone, stopped listening, or no longer needs telling.
		d.Store.DeleteAppOutbox(r.ID)
		return
	}
	err = d.send(ctx, iss, client, r)
	if err == nil {
		d.Store.DeleteAppOutbox(r.ID)
		return
	}
	next := d.now().Add(backoff.Jitter(backoff.Exp(15*time.Second, time.Hour, r.Attempts)))
	d.Store.RetryAppOutbox(r.ID, next, err.Error())
	if age > stuckAfter && !r.Noticed && d.Notice != nil {
		if d.Notice("signin-logout", client.ClientID,
			fmt.Sprintf("%s hasn't heard that someone signed out", client.ClientID),
			fmt.Sprintf("fog has been trying for a day to tell %s that a session ended (%s). Until it hears, that session may stay open in %s — at the latest until the fog session it came from would have expired, which every ID token states (fog_session_exp) and fog-client honours.", client.ClientID, err, client.ClientID)) == nil {
			d.Store.MarkAppOutboxNoticed(r.ID)
		}
	}
}

// send signs and posts one logout token.
func (d *Deliverer) send(ctx context.Context, iss string, client *store.OIDCClient, r store.AppOutboxRow) error {
	var p struct {
		SID string `json:"sid"`
		UID string `json:"uid"`
		All int64  `json:"all"`
	}
	if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
		return err
	}
	if p.All != 0 {
		tok, err := d.Keys.Sign(oidc.LogoutAllToken(iss, client.ClientID, r.IdemKey, time.Unix(p.All, 0), d.now()), "logout+jwt")
		if err != nil {
			return err
		}
		return d.post(ctx, client, tok)
	}
	sub := ""
	if p.UID != "" {
		inst, err := d.Instance()
		if err != nil {
			return err
		}
		sub = identity.Address(inst, p.UID)
	}
	tok, err := d.Keys.Sign(oidc.LogoutToken(iss, client.ClientID, sub, p.SID, r.IdemKey, d.now()), "logout+jwt")
	if err != nil {
		return err
	}
	return d.post(ctx, client, tok)
}

// post delivers one signed logout token to the app's back channel.
func (d *Deliverer) post(ctx context.Context, client *store.OIDCClient, tok string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", client.BackchannelURI, strings.NewReader(url.Values{"logout_token": {tok}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := noRedirects(d.HTTP, 10*time.Second).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	// OIDC Back-Channel Logout §2.8: 200 is success; anything else is not —
	// a redirect included, which is never followed (noRedirects).
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s answered %s", client.BackchannelURI, res.Status)
	}
	return nil
}

// deliverVisit sends one visit logout to the peer that hosted the session.
// A peer that stays unreachable stops mattering when its visitor session
// would have expired on its own.
func (d *Deliverer) deliverVisit(ctx context.Context, r store.AppOutboxRow, age time.Duration) {
	peerID, err := strconv.ParseInt(strings.TrimPrefix(r.Target, "peer:"), 10, 64)
	var p struct {
		SID string `json:"sid"`
	}
	if err != nil || d.Visit == nil || json.Unmarshal([]byte(r.Payload), &p) != nil || p.SID == "" || age > visitGiveUpAfter {
		d.Store.DeleteAppOutbox(r.ID)
		return
	}
	if err := d.Visit(ctx, peerID, p.SID); err != nil {
		d.Store.RetryAppOutbox(r.ID, d.now().Add(backoff.Jitter(backoff.Exp(15*time.Second, time.Hour, r.Attempts))), err.Error())
		return
	}
	d.Store.DeleteAppOutbox(r.ID)
}

// visitGiveUpAfter: a host caps a visitor session at 12 hours, so after a
// day no visit is left to end.
const visitGiveUpAfter = 24 * time.Hour

// noRedirects is hc (or a default client with timeout) that never follows a
// redirect: what fog sends an app — a logout token, a SCIM bearer — goes to
// the address the app declared and nowhere it points onward. A hostile app
// answering 3xx could otherwise aim fogd at internal addresses, or at a
// server that answers 200 for it. The 3xx itself comes back as the
// response, and counts as a failed delivery.
func noRedirects(hc *http.Client, timeout time.Duration) *http.Client {
	c := http.Client{Timeout: timeout}
	if hc != nil {
		c = *hc
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}
