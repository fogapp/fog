package flare

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Event is the subset of Sentry's event payload that grouping, display, and
// lifecycle need. Everything else in the payload is stored verbatim and
// rendered from JSON in the UI — parsing it here would only be a place for
// bugs to live.
type Event struct {
	EventID     string          `json:"event_id"`
	Platform    string          `json:"platform,omitempty"`
	Level       string          `json:"level,omitempty"`
	Transaction string          `json:"transaction,omitempty"`
	ServerName  string          `json:"server_name,omitempty"`
	Release     string          `json:"release,omitempty"`
	Environment string          `json:"environment,omitempty"`
	Fingerprint []string        `json:"fingerprint,omitempty"`
	Message     EventMessage    `json:"message,omitempty"`
	Exception   ExceptionChain  `json:"exception,omitempty"`
	Logger      string          `json:"logger,omitempty"`
	Request     json.RawMessage `json:"request,omitempty"`
	Tags        TagMap          `json:"tags,omitempty"`
}

// TagMap tolerates both tag encodings (object, or array of [key, value]
// pairs) and coerces values to strings, since tags are string-typed in the
// schema but clients get this wrong.
type TagMap map[string]string

func (t *TagMap) UnmarshalJSON(b []byte) error {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err == nil {
		out := TagMap{}
		for k, v := range obj {
			out[k] = coerceString(v)
		}
		*t = out
		return nil
	}
	var pairs [][2]any
	if err := json.Unmarshal(b, &pairs); err != nil {
		return err
	}
	out := TagMap{}
	for _, p := range pairs {
		out[coerceString(p[0])] = coerceString(p[1])
	}
	*t = out
	return nil
}

func coerceString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// EventMessage tolerates both encodings SDKs use: a bare string and the
// message interface object {"formatted": "..."}.
type EventMessage struct {
	Formatted string
}

func (m *EventMessage) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		m.Formatted = s
		return nil
	}
	var obj struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	m.Formatted = obj.Formatted
	if m.Formatted == "" {
		m.Formatted = obj.Message
	}
	return nil
}

func (m EventMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.Formatted)
}

// ExceptionChain tolerates both encodings: {"values": [...]} and a bare
// array. Values are ordered root cause first, crash site last.
type ExceptionChain struct {
	Values []ExceptionValue
}

func (c *ExceptionChain) UnmarshalJSON(b []byte) error {
	var wrapped struct {
		Values []ExceptionValue `json:"values"`
	}
	if err := json.Unmarshal(b, &wrapped); err == nil && wrapped.Values != nil {
		c.Values = wrapped.Values
		return nil
	}
	return json.Unmarshal(b, &c.Values)
}

func (c ExceptionChain) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Values []ExceptionValue `json:"values"`
	}{c.Values})
}

type ExceptionValue struct {
	Type       string      `json:"type"`
	Value      string      `json:"value"`
	Module     string      `json:"module,omitempty"`
	Mechanism  *Mechanism  `json:"mechanism,omitempty"`
	Stacktrace *Stacktrace `json:"stacktrace,omitempty"`
}

type Mechanism struct {
	Type    string `json:"type,omitempty"`
	Handled *bool  `json:"handled,omitempty"`
}

type Stacktrace struct {
	Frames []Frame `json:"frames"`
}

// Frame: frames are ordered oldest call first, crash site last.
type Frame struct {
	Filename    string   `json:"filename,omitempty"`
	AbsPath     string   `json:"abs_path,omitempty"`
	Function    string   `json:"function,omitempty"`
	Module      string   `json:"module,omitempty"`
	Lineno      int      `json:"lineno,omitempty"`
	InApp       *bool    `json:"in_app,omitempty"`
	ContextLine string   `json:"context_line,omitempty"`
	PreContext  []string `json:"pre_context,omitempty"`
	PostContext []string `json:"post_context,omitempty"`
}

func ParseEvent(payload []byte) (*Event, error) {
	ev := &Event{}
	if err := json.Unmarshal(payload, ev); err != nil {
		return nil, err
	}
	return ev, nil
}

// Crash returns the most recently raised exception — the one whose class and
// stack define the failure. Sentry orders chains root cause first, so it's
// the last value.
func (e *Event) Crash() *ExceptionValue {
	if len(e.Exception.Values) == 0 {
		return nil
	}
	return &e.Exception.Values[len(e.Exception.Values)-1]
}

// ErrorClass is the group's headline class: the crash exception's type, or
// "message" for exception-less reports (Rails.error.report with a string).
func (e *Event) ErrorClass() string {
	if c := e.Crash(); c != nil && c.Type != "" {
		return c.Type
	}
	return "message"
}

const maxTitleLen = 300

// Title is the human line shown in lists: "Class: message", bounded so one
// interpolated SQL statement can't bloat every list query.
func (e *Event) Title() string {
	t := ""
	if c := e.Crash(); c != nil {
		t = c.Type
		if c.Value != "" {
			t += ": " + c.Value
		}
	} else if e.Message.Formatted != "" {
		t = e.Message.Formatted
	} else {
		t = "(no message)"
	}
	if len(t) > maxTitleLen {
		// Back up to a rune boundary: a byte-offset cut can split a
		// multi-byte rune and store invalid UTF-8.
		cut := maxTitleLen
		for cut > 0 && !utf8.RuneStart(t[cut]) {
			cut--
		}
		t = t[:cut] + "…"
	}
	return t
}

// Culprit locates the failure for list views: the transaction (route or job
// name) when the SDK sent one, else the innermost in-app frame.
func (e *Event) Culprit() string {
	if e.Transaction != "" {
		return e.Transaction
	}
	if c := e.Crash(); c != nil && c.Stacktrace != nil {
		frames := c.Stacktrace.Frames
		for i := len(frames) - 1; i >= 0; i-- {
			f := frames[i]
			if f.InApp != nil && *f.InApp {
				return frameLabel(f)
			}
		}
		if len(frames) > 0 {
			return frameLabel(frames[len(frames)-1])
		}
	}
	return ""
}

func frameLabel(f Frame) string {
	name := f.Filename
	if name == "" {
		name = f.AbsPath
	}
	if f.Function != "" {
		return f.Function + " (" + name + ")"
	}
	return name
}

// Unhandled reports whether any exception in the chain was an unrescued
// crash (mechanism.handled == false). Absent mechanisms count as handled:
// claiming a crash we can't prove would be the wrong kind of alarming.
func (e *Event) Unhandled() bool {
	for _, v := range e.Exception.Values {
		if v.Mechanism != nil && v.Mechanism.Handled != nil && !*v.Mechanism.Handled {
			return true
		}
	}
	return false
}

// NormLevel maps the event level onto the five Sentry levels, defaulting to
// error — an unlabeled report reached an error tracker, after all.
func (e *Event) NormLevel() string {
	switch strings.ToLower(e.Level) {
	case "fatal", "warning", "info", "debug":
		return strings.ToLower(e.Level)
	default:
		return "error"
	}
}
