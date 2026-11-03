package signin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// SCIM provisioning (docs/signin-contract.md). fog is a SCIM 2.0 client
// (RFC 7643/7644) of each app that declares signin.scim, and keeps it in
// step with who may use it by reconciling: compute who the app's policy
// lets in and what fog would say about each, compare with what it last
// said, send only the difference. Converges from any state — a lost row is
// found again by externalId — and never deletes: someone no longer allowed
// becomes active:false, and what they made stays the app's. Someone deleted
// from fog leaves a scim_departures row (0073), kept until the app has heard
// they are inactive, so a removal survives restarts and unreachable apps.
// A client that keeps failing is the operator's business: after StuckAfter
// the Inbox says so, and the notice goes when the app hears again.

// scimSecretName is the app-scoped vault secret the app's SCIM endpoint
// accepts as its bearer (injected as SCIM_TOKEN).
const scimSecretName = "_scim_token"

const userSchema = "urn:ietf:params:scim:schemas:core:2.0:User"

// errSCIMGone is a PUT to an id the app no longer has.
var errSCIMGone = errors.New("scim: the app no longer has that user")

// SCIMUser is the subset of the core User resource fog sends.
type SCIMUser struct {
	Schemas     []string       `json:"schemas"`
	ID          string         `json:"id,omitempty"`
	ExternalID  string         `json:"externalId"`
	UserName    string         `json:"userName"`
	DisplayName string         `json:"displayName"`
	Active      bool           `json:"active"`
	Emails      []SCIMValue    `json:"emails,omitempty"`
	Roles       []SCIMValue    `json:"roles,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`
}

type SCIMValue struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
}

// Desired is who should exist in an app, and how, by person id.
func Desired(people []store.Person, pol store.SigninPolicy, c *store.OIDCClient, instance string) map[int64]SCIMUser {
	out := map[int64]SCIMUser{}
	for _, p := range people {
		// Provisioning is about who, not how well they proved it: a passkey
		// requirement is met at sign-in, not here.
		d := Access(p, pol, c)
		if d.Outcome == identity.Deny {
			continue
		}
		u := SCIMUser{Schemas: []string{userSchema}, ExternalID: identity.Address(instance, p.UID), UserName: p.Handle,
			DisplayName: p.DisplayName, Active: true, Roles: []SCIMValue{{Value: d.Role, Primary: true}}}
		if u.DisplayName == "" {
			u.DisplayName = p.Handle
		}
		if p.Email != "" && slices.Contains(c.Scopes, "email") {
			u.Emails = []SCIMValue{{Value: p.Email, Primary: true}}
		}
		out[p.ID] = u
	}
	return out
}

// digest is what "the same attributes" means: the user as sent, minus the
// app's id.
func digest(u SCIMUser) string {
	u.ID, u.Meta = "", nil
	b, _ := json.Marshal(u)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Provisioner runs reconciles.
type Provisioner struct {
	Store    *store.Store
	Vault    *vault.Vault
	HTTP     *http.Client
	Log      *slog.Logger
	Instance func() (string, error)
	// Now is the clock (tests move it); StuckAfter is how long a client may
	// keep failing before the Inbox says so (default scimStuckAfter).
	Now        func() time.Time
	StuckAfter time.Duration

	mu sync.Mutex
}

// scimStuckAfter: a reconcile runs every five minutes, so an hour of
// failures is a dozen in a row — an app that is down, not one restarting —
// and someone fog removed may still be active there.
const scimStuckAfter = time.Hour

// noticeKind is the Inbox notice for a client provisioning keeps failing.
const scimNoticeKind = "signin-scim"

func (p *Provisioner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// scimToken is the SCIM bearer for an app, minted on first use. The
// registry hands it to the app as SCIM_TOKEN and the provisioner presents
// it, so both must read the one secret.
func scimToken(secrets vault.Secrets, appID int64) (string, error) {
	return secrets.GetOrMint(appID, scimSecretName, func() (string, error) { return randx.Token(), nil })
}

// Token is the SCIM bearer for an app, minted once.
func (p *Provisioner) Token(app *store.App) (string, error) {
	return scimToken(p.secrets(), app.ID)
}

func (p *Provisioner) secrets() vault.Secrets { return vault.Secrets{Store: p.Store, Vault: p.Vault} }

// Run reconciles every app that takes provisioning.
func (p *Provisioner) Run(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clients, err := p.Store.SCIMClients()
	if err != nil {
		p.Log.Error("scim", "err", err)
		return
	}
	if len(clients) == 0 {
		return
	}
	// The directory and the instance name are the same for every client,
	// so one read serves the whole pass.
	instance, err := p.Instance()
	if err != nil {
		p.Log.Error("scim", "err", err)
		return
	}
	people, err := p.Store.ListPeople()
	if err != nil {
		p.Log.Error("scim", "err", err)
		return
	}
	for i := range clients {
		if ctx.Err() != nil {
			return
		}
		err := p.reconcile(ctx, &clients[i], instance, people)
		if ctx.Err() != nil {
			return // shutting down is not the app failing
		}
		p.health(&clients[i], err)
	}
}

// health records how a reconcile went, and tells the operator — once — when
// a client has been failing for longer than StuckAfter, taking the notice
// back when it works again.
func (p *Provisioner) health(c *store.OIDCClient, rerr error) {
	if rerr == nil {
		noticed, err := p.Store.SCIMHealthy(c.ClientID)
		if err == nil && noticed {
			p.Store.DismissNoticeByKind(scimNoticeKind, c.ClientID)
		}
		return
	}
	p.Log.Warn("scim reconcile", "app", c.ClientID, "err", rerr)
	since, noticed, err := p.Store.SCIMFailing(c.ClientID, rerr.Error())
	if err != nil {
		p.Log.Error("scim health", "app", c.ClientID, "err", err)
		return
	}
	stuck := p.StuckAfter
	if stuck == 0 {
		stuck = scimStuckAfter
	}
	if noticed || p.now().Sub(since) < stuck {
		return
	}
	if err := p.Store.CreateNotice(scimNoticeKind, c.ClientID,
		fmt.Sprintf("%s hasn't heard who may use it", c.ClientID),
		fmt.Sprintf("fog has been failing since %s to tell %s who may use it (%s). Until it hears, someone removed from fog or no longer allowed in %s may still be active there. fog keeps trying every five minutes.",
			since.Local().Format("Jan 2 15:04"), c.ClientID, rerr, c.ClientID)); err == nil {
		p.Store.MarkSCIMNoticed(c.ClientID)
	}
}

func (p *Provisioner) reconcile(ctx context.Context, c *store.OIDCClient, instance string, people []store.Person) error {
	app, err := p.Store.AppByID(c.AppID)
	if err != nil {
		return err
	}
	token, err := p.Token(app)
	if err != nil {
		return err
	}
	pol, err := p.Store.SigninPolicyFor(c.AppID)
	if err != nil {
		return err
	}
	want := Desired(people, *pol, c, instance)
	have, err := p.Store.SCIMStates(c.ClientID)
	if err != nil {
		return err
	}
	byID := map[int64]store.Person{}
	for _, pr := range people {
		byID[pr.ID] = pr
	}
	// Everyone fog should be telling the app about: the allowed, and anyone
	// it told before who is no longer allowed (to switch them off).
	ids := []int64{}
	for id := range want {
		ids = append(ids, id)
	}
	for id, st := range have {
		if _, ok := want[id]; !ok && st.Active {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	// People deleted from fog: switched off before anything else, and
	// forgotten only once the app has heard.
	gone, err := p.Store.SCIMDepartures(c.ClientID)
	if err != nil {
		return err
	}
	for _, d := range gone {
		u := SCIMUser{Schemas: []string{userSchema}, ExternalID: identity.Address(instance, d.PersonUID), UserName: d.UserName,
			DisplayName: d.DisplayName, Active: false}
		if _, err := p.send(ctx, c.SCIMBaseURL, token, d.SCIMID, u); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := p.Store.DeleteSCIMDeparture(c.ClientID, d.PersonUID); err != nil {
			return err
		}
	}
	for _, id := range ids {
		u, allowed := want[id]
		if !allowed {
			pr, ok := byID[id]
			if !ok {
				continue // deleted since the directory was read: its departure row says it
			}
			u = SCIMUser{Schemas: []string{userSchema}, ExternalID: identity.Address(instance, pr.UID), UserName: pr.Handle,
				DisplayName: pr.DisplayName, Active: false}
		}
		st, known := have[id]
		if known && st.Sent == digest(u) {
			continue
		}
		scimID := ""
		if known {
			scimID = st.SCIMID
		}
		newID, err := p.send(ctx, c.SCIMBaseURL, token, scimID, u)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := p.Store.PutSCIMState(store.SCIMState{ClientID: c.ClientID, PersonID: id, SCIMID: newID, Sent: digest(u), Active: u.Active}); err != nil {
			return err
		}
	}
	return firstErr
}

// send creates or replaces one user, finding them by externalId first when
// fog has no id for them (a lost row, or an app restored from backup).
func (p *Provisioner) send(ctx context.Context, base, token, scimID string, u SCIMUser) (string, error) {
	return p.sendOnce(ctx, base, token, scimID, u, false)
}

// sendOnce is send; refound says this is already the retry after a 404, so
// an app that answers its own lookup with an id it then denies cannot loop.
func (p *Provisioner) sendOnce(ctx context.Context, base, token, scimID string, u SCIMUser, refound bool) (string, error) {
	if scimID == "" {
		var list struct {
			Resources []SCIMUser `json:"Resources"`
		}
		filter := fmt.Sprintf(`externalId eq "%s"`, u.ExternalID)
		if err := p.do(ctx, "GET", base+"/Users?filter="+url.QueryEscape(filter), token, nil, &list, http.StatusOK); err != nil {
			return "", err
		}
		if len(list.Resources) > 0 {
			scimID = list.Resources[0].ID
		}
	}
	var out SCIMUser
	if scimID == "" {
		if !u.Active {
			return "", nil // never create someone only to switch them off
		}
		if err := p.do(ctx, "POST", base+"/Users", token, u, &out, http.StatusCreated); err != nil {
			return "", err
		}
	} else {
		u.ID = scimID
		err := p.do(ctx, "PUT", base+"/Users/"+url.PathEscape(scimID), token, u, &out, http.StatusOK)
		if errors.Is(err, errSCIMGone) && !refound {
			// The app no longer has that id — its admin removed the user, or
			// it folded a provisioned placeholder into an older account.
			// Forget the id and find the person again by externalId.
			return p.sendOnce(ctx, base, token, "", u, true)
		}
		if err != nil {
			return "", err
		}
	}
	if out.ID == "" {
		return "", fmt.Errorf("%s answered without an id", base)
	}
	return out.ID, nil
}

func (p *Provisioner) do(ctx context.Context, method, target, token string, body, out any, want int) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/scim+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/scim+json")
	}
	// Never follow a redirect (noRedirects): the bearer goes to the app's
	// declared endpoint and nowhere else, and a 3xx is a failure below.
	res, err := noRedirects(p.HTTP, 15*time.Second).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound && method == "PUT" {
		return errSCIMGone
	}
	if res.StatusCode != want {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, target, res.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}
