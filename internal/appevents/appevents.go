// Package appevents is the household event stream (docs/events-contract.md):
// apps, and fogd itself, publish what happened — a download finished, an
// incident opened, a deploy failed — and a consumer such as HUD reads the
// stream forward from a cursor and decides what each event deserves.
//
// Fog only carries events. It never interprets them, never pushes them
// anywhere, and an app that never publishes loses nothing. Rows live in the
// store for a week (internal/retention); this package validates what comes
// in and wakes long-polling readers when something new lands.
package appevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// FogSource is the source name of events fogd publishes itself. "fog" is a
// reserved app name, so no app can publish as fogd.
const FogSource = "fog"

// Limits on what one publish may carry. Events are notices, not payloads:
// a consumer that needs the whole object asks the app for it.
const (
	MaxBatch    = 100
	MaxDataSize = 32 << 10
	maxIDLen    = 128
	maxSubject  = 256
	// MaxRead caps one page of the stream.
	MaxRead = 500
)

var typeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Event is one event as publishers send it and readers receive it: the
// CloudEvents 1.0 JSON format, plus the stream's own seq extension.
type Event struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject,omitempty"`
	Time            string          `json:"time,omitempty"`
	DataContentType string          `json:"datacontenttype,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
	// Seq is the stream cursor, set on read. Publishers leave it out.
	Seq int64 `json:"seq,omitempty"`
}

// Bus publishes into the store and wakes readers. The zero value is not
// usable; build one with New.
type Bus struct {
	Store *store.Store
	Now   func() time.Time

	mu      sync.Mutex
	changed chan struct{}
}

func New(st *store.Store) *Bus {
	return &Bus{Store: st, Now: time.Now, changed: make(chan struct{})}
}

// ErrInvalid wraps every rejection of a publisher's input, so the handler
// can answer 400 for it and 500 for everything else.
var ErrInvalid = errors.New("invalid event")

func invalid(i int, format string, args ...any) error {
	return fmt.Errorf("%w %d: %s", ErrInvalid, i, fmt.Sprintf(format, args...))
}

// Publish validates evs and appends them to the stream as source. The
// source a publisher sends is ignored: it is whoever the token says it is.
// Returns how many were new (a retried id is not).
func (b *Bus) Publish(source string, evs []Event) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	if len(evs) > MaxBatch {
		return 0, fmt.Errorf("%w: at most %d events per publish", ErrInvalid, MaxBatch)
	}
	now := b.Now().UTC()
	rows := make([]store.AppEvent, 0, len(evs))
	for i, e := range evs {
		if !typeRe.MatchString(e.Type) {
			return 0, invalid(i, "type %q must be a lowercase dotted name like app.thing.happened", e.Type)
		}
		if e.ID == "" {
			e.ID = randx.Hex(16)
		}
		if len(e.ID) > maxIDLen {
			return 0, invalid(i, "id longer than %d", maxIDLen)
		}
		if len(e.Subject) > maxSubject {
			return 0, invalid(i, "subject longer than %d", maxSubject)
		}
		at := now
		if e.Time != "" {
			t, err := time.Parse(time.RFC3339Nano, e.Time)
			if err != nil {
				return 0, invalid(i, "time %q is not RFC 3339", e.Time)
			}
			at = t.UTC()
		}
		if len(e.Data) > MaxDataSize {
			return 0, invalid(i, "data larger than %d bytes", MaxDataSize)
		}
		if len(e.Data) > 0 && !json.Valid(e.Data) {
			return 0, invalid(i, "data is not JSON")
		}
		rows = append(rows, store.AppEvent{
			Source: source, ID: e.ID, Type: e.Type, Subject: e.Subject,
			Time: at.Format(time.RFC3339Nano), Data: e.Data,
		})
	}
	n, err := b.Store.InsertAppEvents(rows)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		b.wake()
	}
	return n, nil
}

// PublishFog is how fogd reports its own events. Failures are the caller's
// to log; publishing must never break the operation being reported.
func (b *Bus) PublishFog(typ, subject string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = b.Publish(FogSource, []Event{{Type: typ, Subject: subject, Data: raw}})
	return err
}

func (b *Bus) wake() {
	b.mu.Lock()
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

func (b *Bus) signal() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

// Page is one read of the stream. Cursor is what to pass as after next
// time; Gap says events between after and the first returned one were
// pruned before this reader saw them.
type Page struct {
	Events []Event `json:"events"`
	Cursor int64   `json:"cursor"`
	Gap    bool    `json:"gap,omitempty"`
}

// Read returns events after the cursor, waiting up to wait for one to
// arrive when there are none yet. after < 0 means "from now": no events,
// just the cursor to start from.
func (b *Bus) Read(ctx context.Context, after int64, limit int, wait time.Duration) (Page, error) {
	if limit <= 0 || limit > MaxRead {
		limit = MaxRead
	}
	oldest, newest, err := b.Store.AppEventBounds()
	if err != nil {
		return Page{}, err
	}
	if after < 0 {
		return Page{Events: []Event{}, Cursor: newest}, nil
	}
	page := Page{Gap: after > 0 && oldest > after+1}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		// Take the signal before querying: an event landing between the
		// query and the wait then still wakes this reader.
		sig := b.signal()
		rows, err := b.Store.AppEventsAfter(after, limit)
		if err != nil {
			return Page{}, err
		}
		if len(rows) > 0 || wait <= 0 {
			page.Events = make([]Event, 0, len(rows))
			page.Cursor = after
			for _, r := range rows {
				page.Events = append(page.Events, fromRow(r))
				page.Cursor = r.Seq
			}
			return page, nil
		}
		select {
		case <-sig:
		case <-deadline.C:
			wait = 0
		case <-ctx.Done():
			return Page{Events: []Event{}, Cursor: after}, nil
		}
	}
}

func fromRow(r store.AppEvent) Event {
	e := Event{
		SpecVersion: "1.0", ID: r.ID, Source: r.Source, Type: r.Type,
		Subject: r.Subject, Time: r.Time, Data: r.Data, Seq: r.Seq,
	}
	if len(r.Data) > 0 {
		e.DataContentType = "application/json"
	}
	return e
}
