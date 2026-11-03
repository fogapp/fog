package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// Notifications (docs/chat-contract.md) carry no content, because fog
// has none: a push says which room and who, and — with a mention hint — that
// you were named. Each person's own fog notifies them, for rooms hubbed here
// and for replicas alike, honoring the per-room setting, the mention hints,
// and their do-not-disturb window.

// Prefs is a person's chat preferences, stored as JSON on their row.
type Prefs struct {
	// DND is a daily quiet window in the person's local time ("22:00"–"07:00").
	DND struct {
		Enabled bool   `json:"enabled"`
		Start   string `json:"start"`
		End     string `json:"end"`
		TZ      string `json:"tz"`
	} `json:"dnd"`
	// Receipts: send encrypted read receipts into rooms.
	Receipts bool `json:"receipts"`
	// Previews: ask fog to fetch link previews (fog then sees the link).
	Previews bool `json:"previews"`
	// NtfyTopic: a personal ntfy topic on the household's ntfy server.
	NtfyTopic string `json:"ntfyTopic"`
	// PushEnabled: deliver Web Push to this person's subscribed devices.
	PushEnabled bool `json:"pushEnabled"`
}

func ParsePrefs(s string) Prefs {
	var p Prefs
	if s != "" {
		json.Unmarshal([]byte(s), &p)
	}
	return p
}

func (p Prefs) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// quiet says whether now falls inside the DND window.
func (p Prefs) quiet(now time.Time) bool {
	if !p.DND.Enabled || p.DND.Start == "" || p.DND.End == "" {
		return false
	}
	loc := time.Local
	if p.DND.TZ != "" {
		if l, err := time.LoadLocation(p.DND.TZ); err == nil {
			loc = l
		}
	}
	t := now.In(loc)
	cur := t.Hour()*60 + t.Minute()
	start, ok1 := hhmm(p.DND.Start)
	end, ok2 := hhmm(p.DND.End)
	if !ok1 || !ok2 {
		return false
	}
	if start <= end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end // crosses midnight
}

func hhmm(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// PushPayload is what a device's service worker receives: metadata only.
type PushPayload struct {
	Kind       string `json:"kind"` // message | mention
	RoomID     string `json:"roomId"`
	Label      string `json:"label"`
	Sender     string `json:"sender"`
	SenderName string `json:"senderName"`
	Seq        int64  `json:"seq"`
}

// Notifier delivers pushes. Sends are swappable for tests.
type Notifier struct {
	Svc   *Service
	Vault *vault.Vault
	// Ntfy sends one ntfy notification to a topic; nil disables.
	Ntfy func(topic, title, body string)
	// SendPush is the Web Push transport; the default speaks RFC 8291 via
	// webpush-go. Tests replace it.
	SendPush func(sub store.PushSub, payload []byte) error
	// NameFor renders a person address for a title.
	NameFor func(addr string) string

	// vapidMu guards the keypair: pushes go out on their own goroutines,
	// and two minting at once would each store a different pair, breaking
	// every subscription made against the loser.
	vapidMu                   sync.Mutex
	vapidPublic, vapidPrivate string
}

// SecretVAPID is the vault secret holding the VAPID keypair.
const SecretVAPID = "_chat_vapid"

// VAPIDPublicKey returns (minting on first use) the public key browsers
// subscribe with.
func (n *Notifier) VAPIDPublicKey() (string, error) {
	pub, _, err := n.vapidKeys()
	return pub, err
}

// vapidKeys loads, or on first use mints and stores, the VAPID keypair.
func (n *Notifier) vapidKeys() (public, private string, err error) {
	n.vapidMu.Lock()
	defer n.vapidMu.Unlock()
	if n.vapidPublic != "" {
		return n.vapidPublic, n.vapidPrivate, nil
	}
	secrets := vault.Secrets{Store: n.Svc.Store, Vault: n.Vault}
	pt, err := secrets.GetOrMint(0, SecretVAPID, mintVAPID)
	if err != nil {
		return "", "", err
	}
	var k struct{ Public, Private string }
	if json.Unmarshal([]byte(pt), &k) != nil || k.Public == "" {
		// An unreadable pair is replaced: subscriptions made against it
		// can't be served anyway.
		if pt, err = mintVAPID(); err != nil {
			return "", "", err
		}
		if err := secrets.Put(0, SecretVAPID, pt); err != nil {
			return "", "", err
		}
		json.Unmarshal([]byte(pt), &k)
	}
	n.vapidPublic, n.vapidPrivate = k.Public, k.Private
	return k.Public, k.Private, nil
}

// mintVAPID makes a fresh keypair, as the JSON the vault secret holds.
func mintVAPID() (string, error) {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(struct{ Public, Private string }{pub, priv})
	return string(raw), nil
}

func (n *Notifier) defaultPush(sub store.PushSub, payload []byte) error {
	if err := CheckPublicURL(sub.Endpoint, true); err != nil {
		return ErrSubscriptionGone
	}
	pub, priv, err := n.vapidKeys()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh}}, &webpush.Options{
		Subscriber:      "mailto:fog@" + strings.TrimSpace(n.Svc.Household()),
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		TTL:             3600,
		Urgency:         webpush.UrgencyNormal,
		// The endpoint is the subscriber's to choose: fog dials it only on
		// a public address, checked again on the address actually dialed.
		HTTPClient: previewClient(),
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		return ErrSubscriptionGone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push endpoint answered %d", resp.StatusCode)
	}
	return nil
}

// ErrSubscriptionGone: the push service says the subscription no longer
// exists; the transport returns it so the row is dropped.
var ErrSubscriptionGone = errors.New("subscription gone")

// Dispatch notifies this household's people about one event. Called for
// every private message that lands, hubbed here or mirrored.
func (n *Notifier) Dispatch(room *store.ChatRoom, ev *store.ChatEvent) {
	if ev.Kind != EventPrivate {
		return
	}
	parts, err := n.Svc.Store.ChatParticipants(room.ID)
	if err != nil {
		return
	}
	me := n.Svc.instanceOrEmpty()
	var mentions []string
	if ev.Hints != "" {
		var h struct {
			Mentions []string `json:"mentions"`
		}
		json.Unmarshal([]byte(ev.Hints), &h)
		mentions = h.Mentions
	}
	mentioned := func(addr string) bool {
		for _, m := range mentions {
			if m == addr {
				return true
			}
		}
		return false
	}
	senderName := ev.SenderAddr
	if n.NameFor != nil {
		senderName = n.NameFor(ev.SenderAddr)
	}
	// Who is owed a notice is settled from the participant rows alone; the
	// online set and the people behind the survivors are then read once
	// for the lot rather than once per member.
	type recipient struct {
		handle, kind string
	}
	var due []recipient
	var online map[string]bool
	for _, p := range parts {
		if p.LeftSeq != 0 || p.HomeInstance != me || p.PersonAddr == ev.SenderAddr {
			continue
		}
		kind := "message"
		switch p.Notify {
		case "none":
			continue
		case "mentions":
			if !mentioned(p.PersonAddr) {
				continue
			}
			kind = "mention"
		default:
			if mentioned(p.PersonAddr) {
				kind = "mention"
			}
		}
		// Someone reading the room live needs no push.
		if kind != "mention" {
			if online == nil {
				online = map[string]bool{}
				for _, addr := range n.Svc.Bus.OnlinePeople() {
					online[addr] = true
				}
			}
			if online[p.PersonAddr] {
				continue
			}
		}
		_, handle, _ := ParseAddr(p.PersonAddr)
		due = append(due, recipient{handle: handle, kind: kind})
	}
	if len(due) == 0 {
		return
	}
	refs := make([]string, len(due))
	for i, r := range due {
		refs[i] = r.handle
	}
	people, err := n.Svc.Store.NotifyRecipients(refs)
	if err != nil {
		return
	}
	for _, r := range due {
		kind := r.kind
		person, ok := people[r.handle]
		if !ok {
			continue
		}
		prefs := ParsePrefs(person.Prefs)
		if prefs.quiet(time.Now()) && kind != "mention" {
			continue
		}
		label := room.Label
		if label == "" {
			label = "a " + room.Kind
		}
		payload, _ := json.Marshal(PushPayload{Kind: kind, RoomID: room.ID, Label: label, Sender: ev.SenderAddr, SenderName: senderName, Seq: ev.Seq})
		if prefs.PushEnabled {
			for _, sub := range person.Subs {
				go n.push(sub, payload)
			}
		}
		if prefs.NtfyTopic != "" && n.Ntfy != nil {
			title := label
			body := senderName + " sent a message"
			if kind == "mention" {
				body = senderName + " mentioned you"
			}
			go n.Ntfy(prefs.NtfyTopic, title, body)
		}
	}
}

// AccountPayload is a push about a person's own account rather than a
// room (kind "account"): fog telling them their password, passkeys or
// sessions changed. Title and body are fog's words, never anyone's message.
type AccountPayload struct {
	Kind  string `json:"kind"` // account
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// NotifyAccount tells one of this household's people (ref: a uid or
// handle) something about their own account, through the channels they
// chose for chat — Web Push to their devices, their ntfy topic. Quiet hours
// don't hold it back: it is about their security, and rare. It reports
// whether any channel was tried.
func (n *Notifier) NotifyAccount(ref, title, body, url string) bool {
	people, err := n.Svc.Store.NotifyRecipients([]string{ref})
	if err != nil {
		return false
	}
	person, ok := people[ref]
	if !ok {
		return false
	}
	prefs := ParsePrefs(person.Prefs)
	tried := false
	if prefs.PushEnabled && len(person.Subs) > 0 {
		payload, _ := json.Marshal(AccountPayload{Kind: "account", Title: title, Body: body, URL: url})
		for _, sub := range person.Subs {
			go n.push(sub, payload)
		}
		tried = true
	}
	if prefs.NtfyTopic != "" && n.Ntfy != nil {
		go n.Ntfy(prefs.NtfyTopic, title, body)
		tried = true
	}
	return tried
}

func (n *Notifier) push(sub store.PushSub, payload []byte) {
	send := n.SendPush
	if send == nil {
		send = n.defaultPush
	}
	err := send(sub, payload)
	switch {
	case err == nil:
	case errors.Is(err, ErrSubscriptionGone):
		n.Svc.Store.DeletePushSub(sub.Endpoint)
	default:
		n.Svc.Store.NotePushFailure(sub.Endpoint)
		if sub.Failures >= 20 {
			n.Svc.Store.DeletePushSub(sub.Endpoint)
		}
		n.Svc.Log.Debug("chat push", "err", err)
	}
}

// ---- link previews (the sender's device asks, and knows fog sees the link) ----

// Preview is what fog could read from a page's head.
type Preview struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	Site        string `json:"site,omitempty"`
}

var previewDenied = errors.New("that address is not one fog will fetch")

// FetchPreview reads a page's metadata for a link preview. It refuses private
// and local addresses so a room cannot be used to probe the LAN, caps the
// body, and never follows more than a few redirects.
//
// The URL check up front gives a clear refusal; the default client's dial
// guard is what holds, since the name is resolved again to connect and a
// rebinding DNS server can answer differently the second time. A client
// passed in (tests) is copied, never modified, and brings its own dialer.
func FetchPreview(ctx context.Context, rawURL string, client *http.Client, resolve func(host string) ([]string, error)) (*Preview, error) {
	u, err := parsePublicURL(rawURL, resolve)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = previewClient()
	} else {
		c := *client
		client = &c
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if _, err := parsePublicURL(req.URL.String(), resolve); err != nil {
			return err
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fog-chat-preview/1 (+https://github.com)")
	req.Header.Set("Accept", "text/html")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the page answered %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return &Preview{URL: u, Site: hostOf(u)}, nil
	}
	// The head of a page carries its preview; past 1 MiB is not worth reading.
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, 1<<20)); err != nil {
		return nil, err
	}
	pv := parsePreview(buf.String())
	pv.URL = u
	pv.Site = hostOf(u)
	return pv, nil
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return strings.TrimPrefix(u, "www.")
}

// parsePreview pulls og:/twitter:/plain tags out of a document without a
// full HTML parser: the head is small and the tags are regular.
func parsePreview(html string) *Preview {
	pv := &Preview{}
	lower := strings.ToLower(html)
	if i := strings.Index(lower, "<title"); i >= 0 {
		if j := strings.Index(lower[i:], ">"); j >= 0 {
			rest := html[i+j+1:]
			if k := strings.Index(strings.ToLower(rest), "</title>"); k >= 0 {
				pv.Title = strings.TrimSpace(unescape(rest[:k]))
			}
		}
	}
	for _, tag := range metaTags(html) {
		name := strings.ToLower(tag["property"])
		if name == "" {
			name = strings.ToLower(tag["name"])
		}
		content := strings.TrimSpace(unescape(tag["content"]))
		if content == "" {
			continue
		}
		switch name {
		case "og:title", "twitter:title":
			if pv.Title == "" || name == "og:title" {
				pv.Title = content
			}
		case "og:description", "twitter:description", "description":
			if pv.Description == "" || name == "og:description" {
				pv.Description = content
			}
		case "og:image", "twitter:image":
			if pv.Image == "" && (strings.HasPrefix(content, "https://") || strings.HasPrefix(content, "http://")) {
				pv.Image = content
			}
		case "og:site_name":
			pv.Site = content
		}
	}
	if len(pv.Description) > 300 {
		pv.Description = pv.Description[:300] + "…"
	}
	if len(pv.Title) > 200 {
		pv.Title = pv.Title[:200]
	}
	return pv
}

func metaTags(html string) []map[string]string {
	var out []map[string]string
	lower := strings.ToLower(html)
	pos := 0
	for {
		i := strings.Index(lower[pos:], "<meta")
		if i < 0 {
			break
		}
		start := pos + i
		end := strings.Index(lower[start:], ">")
		if end < 0 {
			break
		}
		tag := html[start : start+end]
		attrs := map[string]string{}
		for _, key := range []string{"property", "name", "content"} {
			for _, q := range []string{`"`, `'`} {
				marker := key + "=" + q
				if k := strings.Index(strings.ToLower(tag), marker); k >= 0 {
					rest := tag[k+len(marker):]
					if e := strings.Index(rest, q); e >= 0 {
						attrs[key] = rest[:e]
					}
					break
				}
			}
		}
		if len(attrs) > 0 {
			out = append(out, attrs)
		}
		pos = start + end
	}
	return out
}

func unescape(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&nbsp;", " ")
	return r.Replace(s)
}
