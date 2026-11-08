package glass

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

type logRecord struct {
	T     string         `json:"t"`
	TS    float64        `json:"ts"`
	Lvl   string         `json:"lvl"`
	Msg   string         `json:"msg"`
	Src   string         `json:"src"`
	Trace string         `json:"trace,omitempty"`
	Span  string         `json:"span,omitempty"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// LogHandler passes every record on to next and also ships it to the
// default client when it is at or above the log_level directive Glass
// sent. A line logged with a context inside a transaction carries its
// trace and span, so the trace shows its own logs.
func LogHandler(next slog.Handler) slog.Handler { return &logHandler{next: next} }

type logHandler struct {
	next   slog.Handler
	attrs  []slog.Attr
	groups []string
}

func (h *logHandler) wants(level slog.Level) bool {
	c := Default()
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeLocked() && !c.logOff && level >= c.logLevel
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level) || h.wants(level)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r)
	}
	if !h.wants(r.Level) {
		return err
	}
	rec := &logRecord{T: "log", TS: unixSeconds(r.Time), Lvl: levelName(r.Level), Msg: truncate(r.Message, 8000), Src: "fogd"}
	if r.Time.IsZero() {
		rec.TS = unixSeconds(time.Now())
	}
	if s := scopeOf(ctx); s != nil && s.txn != nil {
		rec.Trace, rec.Span = s.trace, s.span
	}
	attrs := map[string]any{}
	prefix := strings.Join(h.groups, ".")
	for _, a := range h.attrs {
		flatten(attrs, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		flatten(attrs, prefix, a)
		return len(attrs) < maxAttrs
	})
	if len(attrs) > 0 {
		rec.Attrs = attrs
	}
	Default().enqueue("log", rec)
	return err
}

func flatten(into map[string]any, prefix string, a slog.Attr) {
	if len(into) >= maxAttrs {
		return
	}
	a.Value = a.Value.Resolve()
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		for _, g := range a.Value.Group() {
			flatten(into, key, g)
		}
	case slog.KindString:
		into[key] = truncate(a.Value.String(), 1000)
	case slog.KindInt64:
		into[key] = a.Value.Int64()
	case slog.KindUint64:
		into[key] = a.Value.Uint64()
	case slog.KindFloat64:
		into[key] = a.Value.Float64()
	case slog.KindBool:
		into[key] = a.Value.Bool()
	case slog.KindDuration:
		into[key] = millis(a.Value.Duration())
	default:
		into[key] = scalar(a.Value.Any())
	}
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.next = h.next.WithAttrs(attrs)
	prefix := strings.Join(h.groups, ".")
	cp.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		if prefix != "" {
			a.Key = prefix + "." + a.Key
		}
		cp.attrs = append(cp.attrs, a)
	}
	return &cp
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := *h
	cp.next = h.next.WithGroup(name)
	cp.groups = append(append([]string(nil), h.groups...), name)
	return &cp
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError+4:
		return "fatal"
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}
