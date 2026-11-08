package glass

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"
)

// The records, as glass-ingest/1 spells them. Absent optional fields are
// absent, not null.

type txnRecord struct {
	T      string         `json:"t"`
	TS     float64        `json:"ts"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Trace  string         `json:"trace"`
	Span   string         `json:"span"`
	Parent string         `json:"parent,omitempty"`
	Dur    float64        `json:"dur"`
	Status string         `json:"status,omitempty"`
	Error  string         `json:"error,omitempty"`
	Method string         `json:"method,omitempty"`
	Route  string         `json:"route,omitempty"`
	Path   string         `json:"path,omitempty"`
	HTTP   int            `json:"http,omitempty"`
	ExtMS  float64        `json:"ext_ms,omitempty"`
	ExtN   int            `json:"ext_n,omitempty"`
	Kept   bool           `json:"kept,omitempty"`
	Attrs  map[string]any `json:"attrs,omitempty"`
}

type spanRecord struct {
	T      string         `json:"t"`
	TS     float64        `json:"ts"`
	Trace  string         `json:"trace"`
	Span   string         `json:"span"`
	Parent string         `json:"parent,omitempty"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Res    string         `json:"res,omitempty"`
	Dur    float64        `json:"dur"`
	Status string         `json:"status,omitempty"`
	Attrs  map[string]any `json:"attrs,omitempty"`
}

const maxAttrs = 64

func newID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// scope is where a context stands in a trace: the transaction it is part
// of (nil for a remote parent alone) and the span new work hangs from.
type scope struct {
	txn         *Txn
	trace, span string
}

type scopeKey struct{}

func scopeOf(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	return s
}

// WithTraceparent continues a caller's trace: a valid W3C traceparent
// header makes the next transaction started under ctx its child.
func WithTraceparent(ctx context.Context, header string) context.Context {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 || !isHex(parts[1]) || !isHex(parts[2]) ||
		parts[1] == strings.Repeat("0", 32) || parts[2] == strings.Repeat("0", 16) {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, &scope{trace: parts[1], span: parts[2]})
}

// Traceparent is the W3C header for the span ctx stands in, or "".
func Traceparent(ctx context.Context) string {
	s := scopeOf(ctx)
	if s == nil || s.txn == nil {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-01", s.trace, s.span)
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Txn is one unit of work: a request, a job, a deploy. Every method on a
// nil *Txn is a no-op, so call sites never check whether Glass is on.
type Txn struct {
	c     *Client
	start time.Time

	mu    sync.Mutex
	rec   txnRecord
	spans []*spanRecord
	ended bool
}

// Start begins a transaction of kind under the default client, continuing
// whatever trace ctx is in: a transaction started inside another (a
// snapshot inside an update) is its child, as is one started under
// WithTraceparent. The returned ctx carries it to the spans and log lines
// made under it.
func Start(ctx context.Context, kind, name string) (context.Context, *Txn) {
	return Default().Start(ctx, kind, name)
}

// Start is the package-level Start on c.
func (c *Client) Start(ctx context.Context, kind, name string) (context.Context, *Txn) {
	if !c.active() {
		return ctx, nil
	}
	t := &Txn{c: c, start: time.Now()}
	t.rec = txnRecord{T: "txn", Kind: kind, Name: name, Span: newID(8)}
	if s := scopeOf(ctx); s != nil {
		t.rec.Trace, t.rec.Parent = s.trace, s.span
	} else {
		t.rec.Trace = newID(16)
	}
	return context.WithValue(ctx, scopeKey{}, &scope{txn: t, trace: t.rec.Trace, span: t.rec.Span}), t
}

// FromContext is the transaction ctx is in — for a handler, the request
// Middleware started — or nil (inert) outside one.
func FromContext(ctx context.Context) *Txn {
	if s := scopeOf(ctx); s != nil {
		return s.txn
	}
	return nil
}

// SetName renames the transaction — for a request, once routing has said
// which endpoint it was.
func (t *Txn) SetName(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.rec.Name = name
	t.mu.Unlock()
}

// HTTP records the request facet.
func (t *Txn) HTTP(method, route, path string, status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.rec.Method, t.rec.Route, t.rec.Path, t.rec.HTTP = method, route, path, status
	t.mu.Unlock()
}

// Attr sets one attribute: a string, number or bool, never a secret or a
// person's content. Past 64 the rest are dropped.
func (t *Txn) Attr(key string, v any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rec.Attrs == nil {
		t.rec.Attrs = map[string]any{}
	}
	if _, ok := t.rec.Attrs[key]; ok || len(t.rec.Attrs) < maxAttrs {
		t.rec.Attrs[key] = scalar(v)
	}
}

// Fail marks the transaction errored without ending it: a request that
// answered 5xx has no Go error to hand End.
func (t *Txn) Fail(class string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.rec.Status = "error"
	if t.rec.Error == "" {
		t.rec.Error = class
	}
	t.mu.Unlock()
}

// Discard ends the transaction without reporting it — a stream or a
// hijacked connection whose duration is how long someone watched.
func (t *Txn) Discard() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.ended = true
	t.mu.Unlock()
}

// End finishes the transaction. A non-nil err marks it errored, its class
// the error's Go type. The row always goes; its spans go too when it
// errored, was the first of its name this boot, ran slower than its name's
// recent p95, or won the trace_sample_rate draw.
func (t *Txn) End(err error) {
	if t == nil {
		return
	}
	dur := time.Since(t.start)
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended = true
	if err != nil {
		t.rec.Status = "error"
		if t.rec.Error == "" {
			t.rec.Error = errorClass(err)
		}
	}
	rec := t.rec
	spans := t.spans
	t.mu.Unlock()

	rec.TS = unixSeconds(t.start)
	rec.Dur = millis(dur)
	c := t.c
	c.mu.Lock()
	defer c.mu.Unlock()
	keep := c.keepLocked(rec.Kind+"\x00"+rec.Name, rec.Dur, rec.Status == "error")
	rec.Kept = keep && len(spans) > 0
	c.enqueueLocked("txn", &rec)
	if !rec.Kept {
		return
	}
	for _, s := range spans {
		c.enqueueLocked("span", s)
	}
}

// keepLocked is the tail-sampling decision; it also records dur in the
// name's history. Caller holds c.mu.
func (c *Client) keepLocked(name string, dur float64, errored bool) bool {
	d := c.seen[name]
	first := d == nil
	if first {
		if len(c.seen) >= 2000 {
			return errored || mrand.Float64() < c.sampleRate
		}
		d = &durations{}
		c.seen[name] = d
	}
	slow := d.n >= 20 && dur > d.p95()
	d.add(dur)
	return errored || first || slow || mrand.Float64() < c.sampleRate
}

// durations is a name's last hundred durations.
type durations struct {
	ring [100]float64
	n    int
}

func (d *durations) add(v float64) {
	d.ring[d.n%len(d.ring)] = v
	d.n++
}

func (d *durations) p95() float64 {
	n := min(d.n, len(d.ring))
	s := make([]float64, n)
	copy(s, d.ring[:n])
	sort.Float64s(s)
	return s[(n*95)/100]
}

// Span is one step inside a transaction. Every method on a nil *Span is a
// no-op.
type Span struct {
	txn   *Txn
	start time.Time
	rec   spanRecord
}

// StartSpan begins a child span of whatever transaction ctx is in; outside
// one it returns ctx and nil. kind is sql, http, cache, enqueue or custom;
// res the normalized resource (an outbound "METHOD host/path").
func StartSpan(ctx context.Context, kind, name, res string) (context.Context, *Span) {
	s := scopeOf(ctx)
	if s == nil || s.txn == nil {
		return ctx, nil
	}
	sp := &Span{txn: s.txn, start: time.Now(),
		rec: spanRecord{T: "span", Trace: s.trace, Span: newID(8), Parent: s.span, Kind: kind, Name: name, Res: res}}
	return context.WithValue(ctx, scopeKey{}, &scope{txn: s.txn, trace: s.trace, span: sp.rec.Span}), sp
}

// Attr sets one attribute on the span.
func (sp *Span) Attr(key string, v any) {
	if sp == nil {
		return
	}
	if sp.rec.Attrs == nil {
		sp.rec.Attrs = map[string]any{}
	}
	if len(sp.rec.Attrs) < maxAttrs {
		sp.rec.Attrs[key] = scalar(v)
	}
}

// End finishes the span; a non-nil err marks it errored. An http span's
// time also counts toward its transaction's ext_ms.
func (sp *Span) End(err error) {
	if sp == nil {
		return
	}
	dur := time.Since(sp.start)
	sp.rec.TS = unixSeconds(sp.start)
	sp.rec.Dur = millis(dur)
	if err != nil {
		sp.rec.Status = "error"
	}
	t := sp.txn
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return
	}
	if sp.rec.Kind == "http" {
		t.rec.ExtMS += sp.rec.Dur
		t.rec.ExtN++
	}
	if len(t.spans) >= maxSpans {
		t.c.mu.Lock()
		t.c.dropped["span"]++
		t.c.mu.Unlock()
		return
	}
	t.spans = append(t.spans, &sp.rec)
}

// errorClass names an error the way an exception class names a Ruby one:
// by its Go type, which groups like failures without their messages.
func errorClass(err error) string {
	name := fmt.Sprintf("%T", err)
	if name == "*errors.errorString" || name == "*fmt.wrapError" || name == "*fmt.wrapErrors" || name == "*errors.joinError" {
		return "error"
	}
	return strings.TrimPrefix(name, "*")
}

// scalar keeps strings, numbers and bools as they are and spells anything
// else as a string, at most a thousand characters.
func scalar(v any) any {
	switch x := v.(type) {
	case string:
		return truncate(x, 1000)
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return x
	case time.Duration:
		return millis(x)
	case error:
		return truncate(x.Error(), 1000)
	case fmt.Stringer:
		return truncate(x.String(), 1000)
	}
	return truncate(fmt.Sprint(v), 1000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
