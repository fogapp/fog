package lucid

import (
	"context"
	"time"
)

// LLM is one call to a model, as lucid-ingest/1's llm record spells it:
// what Lucid's Models page and model_usage read. Every one is sent — spend
// and latency are only exact if none is sampled away — and none carries a
// prompt or a completion.
type LLM struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Served is the model that answered, when the server says.
	Served string `json:"served,omitempty"`
	// Tier is the app's class of model: local, frontier.
	Tier    string `json:"tier,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Route   string `json:"route,omitempty"`

	In  int64 `json:"in,omitempty"`
	Out int64 `json:"out,omitempty"`
	// Estimated: the server gave no usage.
	Estimated bool     `json:"estimated,omitempty"`
	Cost      *float64 `json:"cost,omitempty"`
	CostSrc   string   `json:"cost_src,omitempty"`

	TTFT       float64 `json:"ttft,omitempty"`
	Concurrent int     `json:"concurrent,omitempty"`
	Stream     bool    `json:"stream,omitempty"`
	Stop       string  `json:"stop,omitempty"`
	Status     string  `json:"status,omitempty"`
	Error      string  `json:"error,omitempty"`

	Attrs map[string]any `json:"attrs,omitempty"`
}

type llmRecord struct {
	T     string  `json:"t"`
	TS    float64 `json:"ts"`
	Trace string  `json:"trace,omitempty"`
	Span  string  `json:"span,omitempty"`
	Dur   float64 `json:"dur"`
	LLM
}

// ModelCall reports one model call that began at start and has just
// ended, under the default client. Inside a transaction it is that
// transaction's call, sharing its trace.
func ModelCall(ctx context.Context, start time.Time, call LLM) {
	Default().ModelCall(ctx, start, call)
}

// ModelCall is the package-level ModelCall on c.
func (c *Client) ModelCall(ctx context.Context, start time.Time, call LLM) {
	if !c.active() {
		return
	}
	rec := &llmRecord{T: "llm", TS: unixSeconds(start), Dur: millis(time.Since(start)), LLM: call}
	if s := scopeOf(ctx); s != nil && s.txn != nil {
		rec.Trace, rec.Span = s.trace, s.span
	}
	for k, v := range rec.Attrs {
		rec.Attrs[k] = scalar(v)
	}
	c.enqueue("llm", rec)
}
