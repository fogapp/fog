// Package logring keeps fogd's most recent log records in memory, beside
// wherever they are already written. fogd logs to stdout and journald owns
// the history; this is the short tail a feedback report attaches, so "what
// was fogd saying just now" travels with the report instead of needing a
// shell on the box.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/ring"
)

// Record is one log line, flattened for display.
type Record struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Attrs string `json:"attrs,omitempty"`
}

// Bounds per record: a log line that embeds a whole deploy log or a stack
// should not let one record crowd out the rest of the tail.
const (
	maxMsg   = 1024
	maxValue = 512
)

// Ring is a fixed-size buffer of the newest records.
type Ring struct {
	buf *ring.Ring[Record]
}

func New(size int) *Ring {
	return &Ring{buf: ring.New[Record](size)}
}

func (r *Ring) add(rec Record) { r.buf.Add(rec) }

// Recent returns up to n of the newest records, oldest first. A nil ring
// holds nothing.
func (r *Ring) Recent(n int) []Record {
	if r == nil {
		return nil
	}
	return r.buf.Recent(n)
}

// Handler tees every record the next handler accepts into the ring.
func (r *Ring) Handler(next slog.Handler) slog.Handler {
	return &handler{ring: r, next: next}
}

type handler struct {
	ring   *Ring
	next   slog.Handler
	prefix string // group path, "a.b."
	attrs  string // pre-rendered WithAttrs
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(h.attrs)
	rec.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, h.prefix, a)
		return true
	})
	h.ring.add(Record{
		Time:  rec.Time.UTC().Format(time.RFC3339),
		Level: rec.Level.String(),
		Msg:   truncate(rec.Message, maxMsg),
		Attrs: strings.TrimSpace(b.String()),
	})
	return h.next.Handle(ctx, rec)
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		writeAttr(&b, h.prefix, a)
	}
	return &handler{ring: h.ring, next: h.next.WithAttrs(as), prefix: h.prefix, attrs: b.String()}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{ring: h.ring, next: h.next.WithGroup(name), prefix: h.prefix + name + ".", attrs: h.attrs}
}

func writeAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			writeAttr(b, p, g)
		}
		return
	}
	v := truncate(fmt.Sprint(a.Value.Any()), maxValue)
	if strings.ContainsAny(v, " \t\n\"=") {
		v = fmt.Sprintf("%q", v)
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
