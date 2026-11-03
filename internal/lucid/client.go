// Package lucid reports fogd to Lucid, the household's observability
// console, over lucid-ingest/1 (lucid:docs/ingest-protocol.md):
// newline-delimited JSON, gzipped, one POST per ten-second window. fogd is
// an app there like any other — its requests and jobs as transactions, the
// outbound calls inside them as spans, its log lines, its Go runtime and
// the host's vitals as metrics.
//
// It keeps the agent rules every Lucid client keeps: a report never blocks
// the caller, a failed send is dropped and never retried, 429 pauses, 401
// stops until the key changes, and what is shed is counted in the next
// header. Until Set gives it an address and a key every call is a cheap
// no-op and nothing is queued.
package lucid

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	window     = 10 // seconds
	maxRecords = 10000
	maxSeries  = 500
	maxSpans   = 500
	agent      = "fog-go/1"
	gamma      = 1.02
	minValue   = 1e-9
	maxBuckets = 2048
)

var defaultClient atomic.Pointer[Client]

// SetDefault makes c the client the package-level calls (Start, StartSpan,
// Count, Gauge, Dist) report through.
func SetDefault(c *Client) { defaultClient.Store(c) }

// Default is the client SetDefault set, or nil (inert).
func Default() *Client { return defaultClient.Load() }

// Client batches and sends. A nil *Client is inert.
type Client struct {
	release string
	started float64
	http    *http.Client

	mu       sync.Mutex
	endpoint string
	key      string
	windows  map[int64]map[string]*series
	recs     []any
	dropped  map[string]int
	pause    time.Time
	halted   bool
	// From the directives in Lucid's last reply.
	sampleRate float64
	logLevel   slog.Level
	logOff     bool
	// Per transaction name: its first sighting this boot, and its recent
	// durations for the slow-outlier test.
	seen map[string]*durations

	gc gcState
}

// New returns an unconfigured client; Set points it at Lucid.
func New(release string) *Client {
	return &Client{
		release:    release,
		started:    float64(time.Now().UnixNano()) / 1e9,
		http:       &http.Client{Timeout: 5 * time.Second},
		windows:    map[int64]map[string]*series{},
		dropped:    map[string]int{},
		sampleRate: 0.1,
		logLevel:   slog.LevelInfo,
		seen:       map[string]*durations{},
	}
}

// Set points the client at Lucid's base URL with an ingest key; either
// empty makes it inert again and forgets what was queued. A new key lifts a
// 401 stop.
func (c *Client) Set(baseURL, key string) {
	if c == nil {
		return
	}
	endpoint := ""
	if baseURL != "" && key != "" {
		endpoint = strings.TrimRight(baseURL, "/") + "/ingest/v1/batch"
	} else {
		key = ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if key != c.key {
		c.halted = false
	}
	c.endpoint, c.key = endpoint, key
	if key == "" {
		c.windows = map[int64]map[string]*series{}
		c.recs = nil
	}
}

// Halted is true once Lucid refused the key (401/403); only a new key
// through Set sends again.
func (c *Client) Halted() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.halted
}

// activeLocked: configured and not stopped. Caller holds mu.
func (c *Client) activeLocked() bool { return c.key != "" && !c.halted }

func (c *Client) active() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeLocked()
}

// enqueue adds one txn/span/log/marker record, or counts it shed.
func (c *Client) enqueue(kind string, rec any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enqueueLocked(kind, rec)
}

func (c *Client) enqueueLocked(kind string, rec any) {
	if !c.activeLocked() {
		return
	}
	if len(c.recs) >= maxRecords {
		c.dropped[kind]++
		return
	}
	c.recs = append(c.recs, rec)
}

// ---- metrics ----

type series struct {
	name, kind, unit string
	tags             map[string]string
	n                int
	sum, min, max    float64
	last             float64
	sketch           map[int]int
}

func seriesKey(kind, name string, tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(kind + "\x00" + name)
	for _, k := range keys {
		b.WriteString("\x00" + k + "=" + tags[k])
	}
	return b.String()
}

func (c *Client) observe(kind, name, unit string, v float64, tags map[string]string) {
	if c == nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	w := time.Now().Unix() / window * window
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.activeLocked() {
		return
	}
	m := c.windows[w]
	if m == nil {
		m = map[string]*series{}
		c.windows[w] = m
	}
	k := seriesKey(kind, name, tags)
	s := m[k]
	if s == nil {
		if len(m) >= maxSeries {
			c.dropped["metric"]++
			return
		}
		s = &series{name: name, kind: kind, unit: unit, tags: tags, min: v, max: v}
		if kind == "dist" {
			s.sketch = map[int]int{}
		}
		m[k] = s
	}
	s.n++
	s.sum += v
	s.min, s.max, s.last = math.Min(s.min, v), math.Max(s.max, v), v
	if s.sketch != nil && v > minValue {
		s.sketch[int(math.Ceil(math.Log(v)/math.Log(gamma)))]++
	}
}

// Count adds n to a counter.
func (c *Client) Count(name string, n float64, tags map[string]string) {
	c.observe("counter", name, "", n, tags)
}

// Gauge records a level, in unit ("bytes", "percent", or "").
func (c *Client) Gauge(name, unit string, v float64, tags map[string]string) {
	c.observe("gauge", name, unit, v, tags)
}

// Dist records one observation of a distribution, in unit ("ms").
func (c *Client) Dist(name, unit string, v float64, tags map[string]string) {
	c.observe("dist", name, unit, v, tags)
}

func (s *series) record(w int64) map[string]any {
	r := map[string]any{"t": "metric", "ts": w, "name": s.name, "kind": s.kind, "n": s.n, "sum": s.sum}
	if s.unit != "" {
		r["unit"] = s.unit
	}
	if len(s.tags) > 0 {
		r["tags"] = s.tags
	}
	if s.kind == "gauge" || s.kind == "dist" {
		r["min"], r["max"], r["last"] = s.min, s.max, s.last
	}
	if s.kind == "dist" {
		idx := make([]int, 0, len(s.sketch))
		for i := range s.sketch {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		if len(idx) > maxBuckets {
			idx = idx[len(idx)-maxBuckets:]
		}
		pairs := make([]int, 0, 2*len(idx))
		for _, i := range idx {
			pairs = append(pairs, i, s.sketch[i])
		}
		r["sk"] = pairs
	}
	return r
}

// ---- sending ----

// Run samples the Go runtime and flushes every window until ctx ends, then
// flushes once more.
func (c *Client) Run(ctx context.Context) {
	if c == nil {
		return
	}
	t := time.NewTicker(window * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.flush(true)
			return
		case <-t.C:
			c.sampleRuntime()
			c.flush(false)
		}
	}
}

// Body builds one batch from every closed window (all of them when final)
// and the queued records, emptying them; nil when there is nothing to send.
// Exported for tests.
func (c *Client) Body(final bool) []byte {
	body, _ := c.build(final)
	return body
}

// build is Body, and how many records of each type the batch carries — what
// is counted shed when it fails to go.
func (c *Client) build(final bool) ([]byte, map[string]int) {
	now := time.Now().Unix()
	c.mu.Lock()
	var recs []any
	ws := make([]int64, 0, len(c.windows))
	for w := range c.windows {
		ws = append(ws, w)
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i] < ws[j] })
	for _, w := range ws {
		if !final && w+window > now {
			continue
		}
		keys := make([]string, 0, len(c.windows[w]))
		for k := range c.windows[w] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			recs = append(recs, c.windows[w][k].record(w))
		}
		delete(c.windows, w)
	}
	carried := map[string]int{"metric": len(recs)}
	for _, r := range c.recs {
		carried[recordType(r)]++
	}
	recs = append(recs, c.recs...)
	c.recs = nil
	dropped := make(map[string]int, len(c.dropped))
	for k, v := range c.dropped {
		dropped[k] = v
	}
	c.mu.Unlock()
	if len(recs) == 0 {
		return nil, nil
	}

	host, _ := os.Hostname()
	process := map[string]any{
		"kind": "fogd", "host": host, "pid": os.Getpid(), "started_at": c.started,
		"runtime": map[string]string{"go": runtime.Version(), "agent": agent},
	}
	if c.release != "" {
		process["release"] = c.release
	}
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.Encode(map[string]any{"v": 1, "process": process, "sent_at": float64(time.Now().UnixNano()) / 1e9, "dropped": dropped})
	for _, r := range recs {
		enc.Encode(r)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw.Bytes())
	zw.Close()
	return gz.Bytes(), carried
}

// discard drops what Body would have sent, counting it shed, so the next
// batch that goes tells Lucid what the pause cost.
func (c *Client) discard(final bool) {
	now := time.Now().Unix()
	c.mu.Lock()
	defer c.mu.Unlock()
	for w, series := range c.windows {
		if !final && w+window > now {
			continue
		}
		c.dropped["metric"] += len(series)
		delete(c.windows, w)
	}
	for _, r := range c.recs {
		c.dropped[recordType(r)]++
	}
	c.recs = nil
}

func recordType(r any) string {
	switch r.(type) {
	case *txnRecord:
		return "txn"
	case *spanRecord:
		return "span"
	case *logRecord:
		return "log"
	case *llmRecord:
		return "llm"
	}
	return "marker"
}

func (c *Client) flush(final bool) {
	c.mu.Lock()
	endpoint, key := c.endpoint, c.key
	paused := time.Now().Before(c.pause)
	stopped := !c.activeLocked()
	c.mu.Unlock()
	if stopped {
		return
	}
	if paused {
		c.discard(final)
		return
	}
	body, carried := c.build(final)
	if body == nil {
		return
	}
	// A batch that does not land is shed; the next header says so.
	shed := func() {
		for k, n := range carried {
			c.dropped[k] += n
		}
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		c.mu.Lock()
		shed()
		c.mu.Unlock()
		return
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := c.http.Do(req)
	if err != nil {
		c.mu.Lock()
		shed()
		c.mu.Unlock()
		return
	}
	defer res.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	c.mu.Lock()
	defer c.mu.Unlock()
	switch res.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		c.applyLocked(reply)
	case http.StatusUnauthorized, http.StatusForbidden:
		shed()
		c.halted = true
	case http.StatusTooManyRequests:
		secs, err := strconv.Atoi(res.Header.Get("Retry-After"))
		if err != nil || secs < 1 || secs > 300 {
			secs = 60
		}
		shed()
		c.pause = time.Now().Add(time.Duration(secs) * time.Second)
	default:
		shed()
	}
}

// applyLocked takes the directives this agent understands from an ingest
// reply and ignores the rest. Caller holds mu.
func (c *Client) applyLocked(reply []byte) {
	var r struct {
		Directives struct {
			TraceSampleRate *float64 `json:"trace_sample_rate"`
			LogLevel        string   `json:"log_level"`
		} `json:"directives"`
	}
	if json.Unmarshal(reply, &r) != nil {
		return
	}
	if v := r.Directives.TraceSampleRate; v != nil && *v >= 0 && *v <= 1 {
		c.sampleRate = *v
	}
	switch r.Directives.LogLevel {
	case "debug":
		c.logLevel, c.logOff = slog.LevelDebug, false
	case "info":
		c.logLevel, c.logOff = slog.LevelInfo, false
	case "warn":
		c.logLevel, c.logOff = slog.LevelWarn, false
	case "error":
		c.logLevel, c.logOff = slog.LevelError, false
	case "off":
		c.logOff = true
	}
}
