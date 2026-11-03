package flare

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// Processor turns accepted event payloads into stored groups and operator
// notifications. One instance serves the whole server; it owns the push
// throttling state.
type Processor struct {
	Store  *store.Store
	Log    *slog.Logger
	Notify func(title, msg string, ok bool)
	// Forward, when set, receives every locally stored event for relaying
	// upstream (fog-to-fog). Only local events pass through it — peer events
	// are never re-forwarded, which makes forwarding cycles structurally
	// impossible rather than TTL-guarded.
	Forward func(appName string, payload []byte)

	pushMu    sync.Mutex
	pushTimes map[string][]time.Time // throttle key -> recent push timestamps
}

// Push flood guard, per throttle key (the app's name, or "peer:<name>" for a
// peer's errors): a deploy that breaks everything creates many *distinct*
// new groups at once, and the per-group throttle does nothing against that.
// Only the ntfy push is capped. Flare files no notices; every event still
// lands in its group on the Inbox's Errors tab, counted, whether or not it
// buzzed a phone.
const (
	pushWindow    = time.Hour
	pushPerWindow = 10
	// A single group re-notifies at most hourly (regression after regression).
	groupNotifyCooldown = time.Hour
)

// maxStoredPayload bounds what lands in SQLite per event; slimPayload
// enforces it.
const maxStoredPayload = 256 << 10

// slimPayload bounds an event payload for storage. Oversized payloads are
// re-marshaled from the parsed subset instead of dropped: the stack trace
// survives, the bloat (giant breadcrumb trails) does not. Request goes
// entirely — it's held verbatim (json.RawMessage), so re-marshaling alone
// wouldn't shrink an 850KB request body, and grouping never reads it. The
// result is hard-checked: the bound is a guarantee, not advice, so a payload
// oversized even after slimming degrades to a stub.
func slimPayload(payload []byte, ev *Event) []byte {
	if len(payload) <= maxStoredPayload {
		return payload
	}
	slim := *ev
	slim.Request = nil
	if b, err := json.Marshal(&slim); err == nil && len(b) <= maxStoredPayload {
		return b
	}
	return []byte(`{"truncated":true}`)
}

// IngestEvent processes one envelope event item for an app. Returns the
// event id for the HTTP response.
func (p *Processor) IngestEvent(app *store.App, payload []byte) (string, error) {
	ev, err := ParseEvent(payload)
	if err != nil {
		return "", fmt.Errorf("event payload: %w", err)
	}
	if ev.EventID == "" {
		// The ID exists to dedupe transport retries; a client that omits it
		// gets a random one (no dedupe, but no rejection either).
		ev.EventID = randx.Hex(16)
	}

	stored := slimPayload(payload, ev)

	res, err := p.Store.IngestError(app.ID, app.Name, ingestFrom(ev, string(stored)))
	if err != nil {
		return "", err
	}
	if res.New || res.Regressed {
		p.notifyTransition(app.Name, app.Name, res)
	}
	if res.Stored && p.Forward != nil {
		p.Forward(app.Name, stored)
	}
	return ev.EventID, nil
}

// IngestPeerEvent processes one event relayed by a peer Fog instance. The
// peer's identity comes from its authenticated token; the payload's
// fog.app tag names only which of the peer's apps broke.
func (p *Processor) IngestPeerEvent(peer *store.FedPeer, payload []byte) (string, error) {
	ev, err := ParseEvent(payload)
	if err != nil {
		return "", fmt.Errorf("event payload: %w", err)
	}
	if ev.EventID == "" {
		ev.EventID = randx.Hex(16)
	}
	appName := ev.Tags["fog.app"]
	if appName == "" {
		appName = "unknown"
	}
	stored := slimPayload(payload, ev)
	res, err := p.Store.IngestPeerError(peer.ID, appName, ingestFrom(ev, string(stored)))
	if err != nil {
		return "", err
	}
	if res.New || res.Regressed {
		p.notifyPeerTransition(peer, appName, res)
	}
	return ev.EventID, nil
}

func ingestFrom(ev *Event, stored string) store.ErrorIngest {
	return store.ErrorIngest{
		Fingerprint: Fingerprint(ev),
		ErrorClass:  ev.ErrorClass(),
		Title:       ev.Title(),
		Culprit:     ev.Culprit(),
		Level:       ev.NormLevel(),
		Unhandled:   ev.Unhandled(),
		EventID:     ev.EventID,
		Release:     ev.Release,
		Environment: ev.Environment,
		Txn:         ev.Transaction,
		Payload:     stored,
	}
}

// notifyTransition fires on the two moments worth an interruption: a group's
// first occurrence and a regression. The ntfy push is throttled per group
// and per throttle key.
//
// It files no notice. The group is already an Inbox row on the Errors tab,
// and the notice this used to write only counted those rows — a second copy
// of the tab's own badge, which had to be dismissed by hand and came back
// with the next error. The durable record is the group; the interruption is
// the push.
func (p *Processor) notifyTransition(appName, throttleKey string, res *store.IngestResult) {
	p.push(fmt.Sprintf("%s in %s", transitionVerb(res), appName), throttleKey, res)
}

// notifyPeerTransition mirrors notifyTransition for a peer's errors, under
// its own push setting.
func (p *Processor) notifyPeerTransition(peer *store.FedPeer, appName string, res *store.IngestResult) {
	// The phone push is a setting (default on — insight is the point of
	// accepting a peer's errors); the group lands on the Errors tab either way.
	if v, _ := p.Store.GetSetting(SettingPeerPush); v == "off" {
		return
	}
	p.push(fmt.Sprintf("%s at %s (%s)", transitionVerb(res), peer.DisplayName(), appName), "peer:"+peer.Name, res)
}

func transitionVerb(res *store.IngestResult) string {
	if res.Regressed {
		return "Error regressed"
	}
	return "New error"
}

func (p *Processor) push(title, throttleKey string, res *store.IngestResult) {
	if p.Notify == nil {
		return
	}
	if t, err := time.Parse(time.RFC3339, res.Group.LastNotified); err == nil && time.Since(t) < groupNotifyCooldown {
		return
	}
	if !p.allowPush(throttleKey) {
		return
	}
	p.Notify(title, res.Group.Title, false)
	if err := p.Store.TouchErrorGroupNotified(res.Group.ID); err != nil {
		p.Log.Warn("flare notify touch", "err", err)
	}
}

func (p *Processor) allowPush(appName string) bool {
	p.pushMu.Lock()
	defer p.pushMu.Unlock()
	if p.pushTimes == nil {
		p.pushTimes = map[string][]time.Time{}
	}
	now := time.Now()
	cutoff := now.Add(-pushWindow)
	kept := p.pushTimes[appName][:0]
	for _, t := range p.pushTimes[appName] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= pushPerWindow {
		p.pushTimes[appName] = kept
		return false
	}
	p.pushTimes[appName] = append(kept, now)
	return true
}
